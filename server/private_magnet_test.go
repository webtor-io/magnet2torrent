package main

import (
	"crypto/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const (
	tr1 = "http://tracker.invalid/0123456789abcdef0123456789abcdef/announce"
	tr2 = "udp://tracker2.invalid:6969/announce"
	def = "http://default.invalid/announce"
)

// seedTorrent seeds one 64 KiB file on loopback and returns its hex
// infohash and the address to put in a magnet's x.pe=. private == nil: the
// info has no private key at all.
func seedTorrent(t *testing.T, private *bool) (ih, peer string) {
	t.Helper()
	cfg := torrent.TestingConfig(t)
	cfg.Seed = true
	// No uTP connection may reach a client in this process. Built with cgo,
	// as tests usually are and the image is not, uTP is libutp, and Close
	// deadlocks with an incoming one: Close holds the client lock and waits
	// for libutp's global lock, which libutp holds while its firewall
	// callback waits for the client lock. Over PEX the seeder told the
	// resolving client its own address, and the client dialled itself.
	cfg.DisablePEX = true
	cfg.DisableUTP = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	// Random content: every call seeds a torrent of its own.
	data := make([]byte, 64<<10)
	_, _ = rand.Read(data)
	path := filepath.Join(cfg.DataDir, "file.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10, Private: private}
	if err := info.BuildFromFilePath(path); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.AddTorrent(&metainfo.MetaInfo{InfoBytes: infoBytes}); err != nil {
		t.Fatal(err)
	}
	return metainfo.HashBytes(infoBytes).HexString(), "127.0.0.1:" + strconv.Itoa(cl.LocalPort())
}

func resolve(t *testing.T, magnet string) (*pb.Magnet2TorrentReply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return newServer([]string{def}).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: magnet})
}

func unmarshalReply(t *testing.T, reply *pb.Magnet2TorrentReply) *metainfo.MetaInfo {
	t.Helper()
	var mi metainfo.MetaInfo
	if err := bencode.Unmarshal(reply.Torrent, &mi); err != nil {
		t.Fatal(err)
	}
	return &mi
}

// A private torrent may be announced only to its own trackers (BEP 27), and
// a magnet without tr= has none: there is nothing to hand out. Nor has one
// whose tr= are blank.
func TestPrivateMagnetWithoutTrackersRefused(t *testing.T) {
	ih, peer := seedTorrent(t, new(true))
	refused := map[string]string{"outcome": outcomePrivateRefused}
	failed := map[string]string{"outcome": outcomeError}
	for name, extra := range map[string]string{"no tr=": "", "blank tr=": "&tr=&tr=%20"} {
		t.Run(name, func(t *testing.T) {
			refusedBefore := counterValue(t, "m2t_resolve_total", refused)
			failedBefore := counterValue(t, "m2t_resolve_total", failed)

			_, err := resolve(t, "magnet:?xt=urn:btih:"+ih+"&x.pe="+peer+extra)
			if c := status.Code(err); c != codes.FailedPrecondition {
				t.Fatalf("code %v (%v), want FailedPrecondition", c, err)
			}
			found := false
			for _, d := range status.Convert(err).Details() {
				if ei, ok := d.(*errdetails.ErrorInfo); ok && ei.Domain == "magnet2torrent" && ei.Reason == "private_magnet" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no ErrorInfo{magnet2torrent, private_magnet} in %v", status.Convert(err).Details())
			}
			if o := resolveOutcome(err); o != outcomePrivateRefused {
				t.Fatalf("outcome %q, want %q", o, outcomePrivateRefused)
			}
			if got := counterValue(t, "m2t_resolve_total", refused) - refusedBefore; got != 1 {
				t.Fatalf("m2t_resolve_total%v: got +%v, want +1", refused, got)
			}
			if got := counterValue(t, "m2t_resolve_total", failed) - failedBefore; got != 0 {
				t.Fatalf("m2t_resolve_total%v: got +%v, want +0", failed, got)
			}
		})
	}
}

// The refusal is the caller's doing, so the logging interceptor writes it at
// Info, not at its default Warn for FailedPrecondition.
func TestPrivateMagnetRefusalLoggedAtInfo(t *testing.T) {
	ih, peer := seedTorrent(t, new(true))
	hook := logtest.NewGlobal()
	defer hook.Reset()
	lis := bufconn.Listen(1 << 20)
	g := newGRPCServer(newServer([]string{def}))
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = pb.NewMagnet2TorrentClient(conn).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: "magnet:?xt=urn:btih:" + ih + "&x.pe=" + peer})
	if c := status.Code(err); c != codes.FailedPrecondition {
		t.Fatalf("code %v (%v), want FailedPrecondition", c, err)
	}
	// The interceptor logs after the reply is sent; GracefulStop waits for it.
	conn.Close()
	g.GracefulStop()

	found := false
	for _, e := range hook.AllEntries() {
		if e.Data["grpc.code"] == codes.FailedPrecondition.String() {
			found = true
			if e.Level != log.InfoLevel {
				t.Errorf("interceptor line at %v, want info", e.Level)
			}
		}
	}
	if !found {
		t.Fatal("no interceptor line with grpc.code FailedPrecondition")
	}
}

// A private magnet with tr= gets exactly its own trackers, in its order and
// with its duplicates, trimmed, blanks dropped: no DEFAULT_TRACKERS, no web
// seeds.
func TestPrivateMagnetKeepsOnlyItsTrackers(t *testing.T) {
	ih, peer := seedTorrent(t, new(true))
	ok := map[string]string{"outcome": outcomeOK}
	okBefore := counterValue(t, "m2t_resolve_total", ok)

	magnet := "magnet:?xt=urn:btih:" + ih + "&x.pe=" + peer + "&tr=" + url.QueryEscape(tr1) +
		"&tr=" + url.QueryEscape(tr2) + "&tr=&tr=" + url.QueryEscape(" "+tr1+" ") + "&ws=" + url.QueryEscape("http://seed.invalid/")
	reply, err := resolve(t, magnet)
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Private {
		t.Fatal("reply.Private = false, want true")
	}
	mi := unmarshalReply(t, reply)
	if want := [][]string{{tr1, tr2, tr1}}; !slices.EqualFunc(mi.AnnounceList, want, slices.Equal) {
		t.Errorf("announce list %q, want %q", mi.AnnounceList, want)
	}
	if mi.Announce != "" {
		t.Errorf("announce %q, want none", mi.Announce)
	}
	if len(mi.UrlList) != 0 {
		t.Errorf("url-list %q, want none", mi.UrlList)
	}
	if got := mi.HashInfoBytes().HexString(); got != ih {
		t.Errorf("infohash %s, want %s", got, ih)
	}
	if got := counterValue(t, "m2t_resolve_total", ok) - okBefore; got != 1 {
		t.Fatalf("m2t_resolve_total%v: got +%v, want +1", ok, got)
	}
}

func TestPublicMagnetUnchanged(t *testing.T) {
	ih, peer := seedTorrent(t, nil)
	reply, err := resolve(t, "magnet:?xt=urn:btih:"+ih+"&x.pe="+peer+"&tr="+url.QueryEscape(tr1))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Private {
		t.Fatal("reply.Private = true, want false")
	}
	al := slices.Concat(unmarshalReply(t, reply).UpvertedAnnounceList()...)
	if !slices.Contains(al, def) || !slices.Contains(al, tr1) {
		t.Fatalf("announce list %q, want both %s and %s", al, def, tr1)
	}
}

// private: 0 in the info is not private (BEP 27 asks for 1).
func TestPrivateZeroIsPublic(t *testing.T) {
	ih, peer := seedTorrent(t, new(false))
	reply, err := resolve(t, "magnet:?xt=urn:btih:"+ih+"&x.pe="+peer)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Private {
		t.Fatal("reply.Private = true, want false")
	}
	if al := slices.Concat(unmarshalReply(t, reply).UpvertedAnnounceList()...); !slices.Contains(al, def) {
		t.Fatalf("announce list %q, want %s in it", al, def)
	}
}

// privateMetainfo on its own: through the handler the announce key is never
// set and web seeds are disabled, so those two guards would not show there.
func TestPrivateMetainfo(t *testing.T) {
	trackers := []string{tr1, tr2}
	mi := privateMetainfo(metainfo.MetaInfo{
		Announce:     def,
		AnnounceList: [][]string{{def}, {tr1}},
		UrlList:      []string{"http://seed.invalid/"},
	}, trackers)
	trackers[0] = def // the result must not share the caller's slice
	if want := [][]string{{tr1, tr2}}; !slices.EqualFunc(mi.AnnounceList, want, slices.Equal) {
		t.Errorf("announce list %q, want %q", mi.AnnounceList, want)
	}
	if mi.Announce != "" {
		t.Errorf("announce %q, want none", mi.Announce)
	}
	if len(mi.UrlList) != 0 {
		t.Errorf("url-list %q, want none", mi.UrlList)
	}
}
