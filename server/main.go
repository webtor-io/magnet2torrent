package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"slices"
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
	"golang.org/x/time/rate"
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
	// allowLocalNetworks lets trackers and metainfo sources be dialled at
	// loopback, private and other non-public addresses (see dialfilter.go).
	// Off by default.
	allowLocalNetworks bool
	// refusalWarn lets one refusal a minute be logged. Shared by the
	// per-request clients, so the limit holds for the process.
	refusalWarn *rate.Limiter
	// Verify: verifyTimeout bounds its started announces together,
	// verifyURLTimeout each one, so that a tracker that never answers leaves
	// time for the next; verifyStopTimeout bounds the stopped sent after an
	// accepted one.
	verifyTimeout, verifyURLTimeout, verifyStopTimeout time.Duration
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
	return &server{
		defaultTrackers:   defaultTrackers,
		refusalWarn:       rate.NewLimiter(rate.Every(time.Minute), 1),
		verifyTimeout:     15 * time.Second,
		verifyURLTimeout:  5 * time.Second,
		verifyStopTimeout: 5 * time.Second,
	}
}

// trackerDialers are what trackers and metainfo sources are dialled with:
// nil, the library's own (a zero net.Dialer and net.ListenPacket), when local
// networks are allowed, otherwise the same plus the check.
func (s *server) trackerDialers() (dial func(ctx context.Context, network, addr string) (net.Conn, error), listen func(network, addr string) (net.PacketConn, error)) {
	if s.allowLocalNetworks {
		return nil, nil
	}
	return (&net.Dialer{Control: s.refuseLocalNetworks}).DialContext, s.listenPacketRefusingLocalNetworks
}

func (s *server) newClient() (*torrent.Client, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.Seed = false
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	// Pods have no IPv6 route. The client splits every udp:// tracker into a
	// udp4 and a udp6 announcer, and the udp6 one failed with "network is
	// unreachable", 80% of all log lines. Also drops the v6 listeners (and so
	// the v6 DHT) and v6 peer dials.
	cfg.DisableIPv6 = true
	cfg.DefaultStorage = noopStorage{}
	// HTTPDialContext serves the metainfo sources (a magnet's xs= and as=),
	// fetched like a tracker from wherever the magnet says.
	cfg.TrackerDialContext, cfg.TrackerListenPacket = s.trackerDialers()
	cfg.HTTPDialContext = cfg.TrackerDialContext
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

	// Neither the magnet nor a parse error is logged or returned (the logging
	// interceptor logs what is returned): a tr= can carry a private tracker's
	// passkey, in the query or in the path. The parse errors quote the input,
	// url.Parse all of it and the infohash parser the xt value, which an
	// escaped separator stretches over the rest of the magnet.
	m, err := metainfo.ParseMagnetV2Uri(in.Magnet)
	if err != nil {
		log.WithField("len", len(in.Magnet)).Error("failed parsing magnet")
		return nil, errors.New("failed parsing magnet")
	}
	log.WithField("infoHash", m.InfoHash.Value.HexString()).Info("processing new request")

	// AddMagnet parses the same way and then panics ("is zero") on a zero v1
	// info-hash: an all-zero btih, or none (v2-only).
	if m.InfoHash.Value.IsZero() {
		return nil, status.Error(codes.InvalidArgument, "magnet has no non-zero btih info-hash")
	}

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
	// BEP 27: a private torrent is announced only to its own trackers. Up to
	// here the resolve announced DEFAULT_TRACKERS too, which only learn the
	// infohash; the metainfo must not hand them on.
	private := t.Info().Private != nil && *t.Info().Private
	if private {
		ih := t.InfoHash().HexString()
		// Trimmed, blanks dropped, as rest-api reads the magnet's tr=: a
		// blank tr= is no tracker.
		var trackers []string
		for _, tr := range m.Trackers {
			if tr = strings.TrimSpace(tr); tr != "" {
				trackers = append(trackers, tr)
			}
		}
		if len(trackers) == 0 {
			log.WithField("infoHash", ih).Info("private magnet refused")
			return nil, errPrivateMagnet
		}
		mi = privateMetainfo(mi, trackers)
		log.WithFields(log.Fields{"infoHash": ih, "urls": len(trackers)}).Info("private magnet resolved with its own trackers")
	}
	bytes, err := bencode.Marshal(mi)
	if err != nil {
		log.WithError(err).Error("failed bencoding torrent metainfo")
		return nil, err
	}
	log.WithField("len", len(bytes)).Info("sending response")
	return &pb.Magnet2TorrentReply{Torrent: bytes, Private: private}, nil
}

// privateMetainfo keeps only the magnet's own trackers, as one tier in the
// magnet's order with its duplicates, and no web seeds. The resolving client
// sets neither the announce key nor web seeds (DisableWebseeds); both are
// cleared anyway, so that the rule does not hang on the client's config.
func privateMetainfo(mi metainfo.MetaInfo, trackers []string) metainfo.MetaInfo {
	mi.Announce = ""
	mi.AnnounceList = [][]string{slices.Clone(trackers)}
	mi.UrlList = nil
	return mi
}

// newGRPCServer builds the server with the full interceptor chain. Shared
// with the tests so that what they measure is what production runs.
func newGRPCServer(srv pb.Magnet2TorrentServer) *grpc.Server {
	grpcLog := log.WithFields(log.Fields{})
	// No payload interceptors: the reply is the whole .torrent, and logging it
	// produced 13 MB lines that Loki rejects (burst 6 MiB) while promtail
	// stalls the whole node's shipping in backoff. Nor the request: the
	// magnet can carry a tracker passkey; the handler logs its infohash.
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

// appFlags is the command line; serverFromFlags builds the server from it.
// Both apart from main so that a test parses what main parses.
func appFlags() []cli.Flag {
	return cs.RegisterPromFlags(cs.RegisterPprofFlags(cs.RegisterProbeFlags([]cli.Flag{
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
		cli.BoolFlag{
			Name:   allowLocalNetworksFlag,
			Usage:  "lets trackers and metainfo sources (xs=, as=) be contacted on local network addresses: loopback, private, link-local, CGNAT, multicast, unspecified, reserved and the rest the README lists (for a tracker on the local network); refused by default, as a torrent or a magnet picks the address",
			EnvVar: allowLocalNetworksEnv,
		},
	})))
}

func serverFromFlags(c *cli.Context) *server {
	s := newServer(parseTrackers(c.String("default-trackers")))
	s.allowLocalNetworks = c.Bool(allowLocalNetworksFlag)
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
	app.Flags = appFlags()
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
		srv := serverFromFlags(c)

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
