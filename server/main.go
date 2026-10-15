package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
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
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type server struct {
	pb.UnimplementedMagnet2TorrentServer
	// defaultTrackers are announced for every magnet on top of its own tr=
	// parameters. Measured 2026-09-04 in prod: magnets carrying 3+ trackers
	// resolved 53% of the time, tracker-less ones 33% — a fresh client has
	// an empty DHT table, and a tracker answers in a round-trip. Same list
	// torrent-store merges into stored torrents (DEFAULT_TRACKERS).
	defaultTrackers []string
}

// maxWait bounds a single resolution regardless of the caller's deadline.
// Ten minutes: web-ui's "try again for 10 minutes" retry must not be cut by
// a shorter server-side cap; the normal first attempt is the caller's 60 s.
const maxWait = 10 * time.Minute

// parseTrackers reads a comma- or whitespace-separated list.
func parseTrackers(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
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

func (noopPiece) ReadAt([]byte, int64) (int, error)      { return 0, io.EOF }
func (noopPiece) WriteAt(b []byte, _ int64) (int, error) { return len(b), nil }
func (noopPiece) MarkComplete() error                    { return nil }
func (noopPiece) MarkNotComplete() error                 { return nil }

// Completion is known (Ok) and incomplete. An unknown completion makes the
// client queue an initial hash check of every piece once the info arrives;
// those hashers outlive Drop and Close, and a v2 piece's length is O(files),
// so a big v2 torrent kept 2+ cores busy after its request had finished.
func (noopPiece) Completion() storage.Completion { return storage.Completion{Ok: true} }

func newServer(defaultTrackers []string) *server {
	return &server{defaultTrackers: defaultTrackers}
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
	// Registered first so it runs last, after the recovery below has set err.
	finishResolve := startResolve()
	defer func() { finishResolve(err) }()

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
	if len(s.defaultTrackers) > 0 {
		t.AddTrackers([][]string{s.defaultTrackers})
	}

	select {
	case <-time.After(maxWait):
		log.WithError(errResolveTooLong).Error("fetching torrent takes too long")
		return nil, errResolveTooLong
	case <-t.GotInfo():
		log.WithFields(log.Fields{
			"infoHash": t.InfoHash(),
			"name":     t.Info().Name,
		}).Info("torrent metainfo fetched")
	case <-ctx.Done():
		log.WithError(ctx.Err()).Error("request deadline exceeded")
		// As a status: the logging interceptor reads a bare context error as
		// Unknown and logs it at error level.
		return nil, status.FromContextError(ctx.Err()).Err()
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

// newGRPCServer builds the server with the full interceptor chain. Shared
// with the tests so that what they measure is what production runs.
func newGRPCServer(srv pb.Magnet2TorrentServer) *grpc.Server {
	grpcLog := log.WithFields(log.Fields{})
	// No payload interceptors: the reply is the whole .torrent, and logging it
	// produced 13 MB lines that Loki rejects (burst 6 MiB) while promtail
	// stalls the whole node's shipping in backoff. The magnet is logged by
	// the handler.
	// The metrics interceptor is outermost so that it observes the final
	// status, including a panic that grpc_recovery (innermost) turned into
	// codes.Internal.
	s := grpc.NewServer(
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpcMetrics.StreamServerInterceptor(),
			grpc_ctxtags.StreamServerInterceptor(),
			grpc_logrus.StreamServerInterceptor(grpcLog),
			grpc_recovery.StreamServerInterceptor(),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpcMetrics.UnaryServerInterceptor(),
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_logrus.UnaryServerInterceptor(grpcLog),
			grpc_recovery.UnaryServerInterceptor(),
		)),
		grpc.MaxRecvMsgSize(grpcMaxMsgSize),
		grpc.MaxSendMsgSize(grpcMaxMsgSize),
		// rest-api holds one connection to the ClusterIP, and kube-proxy
		// balances only at connect, so without an age limit all traffic
		// stayed on one pod for days. At MaxConnectionAge (+-10% jitter) the
		// server sends GOAWAY: new calls go over a fresh connection, which
		// kube-proxy may place on another pod, while calls in flight run on.
		// The grace outlasts maxWait so no resolve is cut.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: maxWait + time.Minute,
		}),
	)
	pb.RegisterMagnet2TorrentServer(s, srv)
	// Zero-valued series for every method, so a dashboard sees 0 instead of
	// "no data" until the first call. Before reflection is registered, so the
	// 17 codes x 2 reflection services are not pre-created as dead series.
	grpcMetrics.InitializeMetrics(s)
	reflection.Register(s)
	return s
}

func main() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	app := cli.NewApp()
	app.Name = "magnet2torrent"
	app.Usage = "runs magnet2torrent server"
	app.Version = "0.0.1"
	app.Flags = cs.RegisterPromFlags(cs.RegisterPprofFlags(cs.RegisterProbeFlags([]cli.Flag{
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
		cli.StringFlag{
			Name:   "default-trackers",
			Usage:  "trackers announced for every magnet on top of its own (comma- or whitespace-separated)",
			EnvVar: "DEFAULT_TRACKERS",
		},
	})))
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

		// Setting Prom (/metrics on 8083, scraped through the chart's ServiceMonitor)
		prom := cs.NewProm(c)
		if prom != nil {
			defer prom.Close()
			go func() {
				if err := prom.Serve(); err != nil {
					log.WithError(err).Error("prom serve error")
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
		srv := newServer(parseTrackers(c.String("default-trackers")))

		grpcError := make(chan error, 1)
		go func() {
			log.WithField("addr", addr).Info("start listening for incoming GRPC connections")
			s := newGRPCServer(srv)
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
