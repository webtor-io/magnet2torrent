package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type server struct {
	pb.UnimplementedMagnet2TorrentServer
}

const grpcMaxMsgSize = 1024 * 1024 * 50

// noopStorage discards all piece data — magnet2torrent only needs metainfo.
type noopStorage struct{}

func (noopStorage) OpenTorrent(_ context.Context, _ *metainfo.Info, _ metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{
		Piece: func(metainfo.Piece) storage.PieceImpl { return noopPiece{} },
	}, nil
}

type noopPiece struct{}

func (noopPiece) ReadAt([]byte, int64) (int, error)  { return 0, io.EOF }
func (noopPiece) WriteAt(b []byte, _ int64) (int, error) { return len(b), nil }
func (noopPiece) MarkComplete() error                { return nil }
func (noopPiece) MarkNotComplete() error             { return nil }
func (noopPiece) Completion() storage.Completion      { return storage.Completion{} }

func newServer() *server {
	return &server{}
}

func (s *server) newClient() (*torrent.Client, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.Seed = false
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.DefaultStorage = noopStorage{}
	return torrent.NewClient(cfg)
}

func (s *server) Magnet2Torrent(ctx context.Context, in *pb.Magnet2TorrentRequest) (reply *pb.Magnet2TorrentReply, err error) {
	// Recover from panics in anacrolix/torrent
	defer func() {
		if r := recover(); r != nil {
			log.WithField("panic", r).Error("recovered from panic in magnet resolution")
			err = status.Errorf(codes.Internal, "internal error: %v", r)
		}
	}()

	log.WithField("magnet", in.Magnet).Info("processing new request")

	// Create a fresh client per request to avoid shared state panics.
	client, err := s.newClient()
	if err != nil {
		log.WithError(err).Error("failed to create torrent client")
		return nil, status.Errorf(codes.Internal, "failed to create client: %v", err)
	}
	defer client.Close()

	t, err := client.AddMagnet(in.Magnet)
	if err != nil {
		log.WithError(err).Error("failed adding new magnet to the client")
		return nil, err
	}
	defer t.Drop()

	select {
	case <-time.After(5 * time.Minute):
		err := status.Error(codes.Aborted, "fetching torrent takes too long")
		log.WithError(err).Error("fetching torrent takes too long")
		return nil, err
	case <-t.GotInfo():
		log.WithFields(log.Fields{
			"infoHash": t.InfoHash(),
			"name":     t.Info().Name,
		}).Info("torrent metainfo fetched")
	case <-ctx.Done():
		log.WithError(ctx.Err()).Error("request deadline exceeded")
		return nil, ctx.Err()
	}

	mi := t.Metainfo()
	bytes, err := bencode.Marshal(mi)
	if err != nil {
		log.WithError(err).Error("failed bencoding torrent metainfo")
		return nil, err
	}
	log.WithField("len", len(bytes)).Info("sending response")
	return &pb.Magnet2TorrentReply{Torrent: bytes}, nil
}

func main() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	app := cli.NewApp()
	app.Name = "magnet2torrent"
	app.Usage = "runs magnet2torrent server"
	app.Version = "0.0.1"
	app.Flags = cs.RegisterPprofFlags(cs.RegisterProbeFlags([]cli.Flag{
		cli.StringFlag{
			Name:   "host, H",
			Usage:  "listening host",
			Value:  "",
			EnvVar: "LISTEN_HOST",
		},
		cli.IntFlag{
			Name:   "port, P",
			Usage:  "listening port",
			Value:  50051,
			EnvVar: "LISTEN_PORT",
		},
	}))
	app.Action = func(c *cli.Context) error {
		// Setting Probe
		probe := cs.NewProbe(c)
		if probe != nil {
			defer probe.Close()
			go func() {
				if err := probe.Serve(); err != nil {
					log.WithError(err).Error("probe serve error")
				}
			}()
		}

		// Setting Pprof
		pprof := cs.NewPprof(c)
		if pprof != nil {
			defer pprof.Close()
			go func() {
				if err := pprof.Serve(); err != nil {
					log.WithError(err).Error("pprof serve error")
				}
			}()
		}

		addr := fmt.Sprintf("%s:%d", c.String("host"), c.Int("port"))
		l, err := net.Listen("tcp", addr)
		defer l.Close()
		if err != nil {
			log.WithError(err).Error("failed to start listening tcp connections")
			return err
		}
		// Setting server (per-request torrent clients)
		srv := newServer()

		grpcError := make(chan error, 1)
		go func() {
			log.WithField("addr", addr).Info("start listening for incoming GRPC connections")
			grpcLog := log.WithFields(log.Fields{})
			alwaysLoggingDeciderServer := func(ctx context.Context, fullMethodName string, servingObject interface{}) bool { return true }
			s := grpc.NewServer(
				grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
					grpc_ctxtags.StreamServerInterceptor(),
					grpc_logrus.StreamServerInterceptor(grpcLog),
					grpc_logrus.PayloadStreamServerInterceptor(grpcLog, alwaysLoggingDeciderServer),
					grpc_recovery.StreamServerInterceptor(),
				)),
				grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
					grpc_ctxtags.UnaryServerInterceptor(),
					grpc_logrus.UnaryServerInterceptor(grpcLog),
					grpc_logrus.PayloadUnaryServerInterceptor(grpcLog, alwaysLoggingDeciderServer),
					grpc_recovery.UnaryServerInterceptor(),
				)),
				grpc.MaxRecvMsgSize(grpcMaxMsgSize),
				grpc.MaxSendMsgSize(grpcMaxMsgSize),
			)
			pb.RegisterMagnet2TorrentServer(s, srv)
			reflection.Register(s)
			err := s.Serve(l)
			grpcError <- err
		}()
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigs:
			log.WithField("signal", sig).Info("got syscall")
		case err := <-grpcError:
			log.WithError(err).Error("got GRPC error")
			return err
		}
		log.Info("shooting down... at last!")
		return nil
	}
	err := app.Run(os.Args)
	if err != nil {
		log.WithError(err).Fatal("failed to serve application")
	}
}
