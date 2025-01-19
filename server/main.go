package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type server struct {
	pb.UnimplementedMagnet2TorrentServer
	// mux sync.Mutex
}

func (s *server) Magnet2Torrent(ctx context.Context, in *pb.Magnet2TorrentRequest) (*pb.Magnet2TorrentReply, error) {
	log.WithField("magnet", in.Magnet).Info("processing new request")
	// s.mux.Lock()
	// defer s.mux.Unlock()
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.ListenPort = 0
	clientConfig.Seed = false
	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		log.WithError(err).Error("failed create torrent client")
		return nil, err
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
	app.Flags = []cli.Flag{
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
	}
	app.Action = func(c *cli.Context) error {
		addr := fmt.Sprintf("%s:%d", c.String("host"), c.Int("port"))
		l, err := net.Listen("tcp", addr)
		defer l.Close()
		if err != nil {
			log.WithError(err).Error("failed to start listening tcp connections")
			return err
		}
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
			)
			pb.RegisterMagnet2TorrentServer(s, &server{})
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
