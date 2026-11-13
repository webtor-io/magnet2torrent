package main

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/tracker"
	"github.com/anacrolix/torrent/version"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const verifyKey = "0123456789abcdef0123456789abcdef"

var verifyIH = strings.Repeat("ab", 20)

// announced is one announce a fake tracker got.
type announced struct {
	event, numwant, infoHash, peerID, key string
	port                                  int
}

type journal struct {
	mu  sync.Mutex
	got []announced
}

func (j *journal) add(a announced) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.got = append(j.got, a)
}

func (j *journal) all() []announced {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.got)
}

func (j *journal) events() string {
	var e []string
	for _, a := range j.all() {
		e = append(e, a.event)
	}
	return strings.Join(e, ",")
}

// fakeHTTPTracker answers every announce: accept with no peers, refuse with
// text as the failure reason, or hang until the client gives up. The key is
// in the URL's path, as private trackers put it.
func fakeHTTPTracker(t *testing.T, mode, text string) (string, *journal) {
	t.Helper()
	j := &journal{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		port, _ := strconv.Atoi(q.Get("port"))
		j.add(announced{
			event: q.Get("event"), numwant: q.Get("numwant"), infoHash: q.Get("info_hash"),
			peerID: q.Get("peer_id"), key: q.Get("key"), port: port,
		})
		var reply map[string]any
		switch mode {
		case "accept":
			reply = map[string]any{"interval": 1800, "peers": ""}
		case "refuse":
			reply = map[string]any{"failure reason": text}
		case "hang":
			<-r.Context().Done()
			return
		}
		b, err := bencode.Marshal(reply)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + verifyKey + "/announce", j
}

// fakeUDPTracker speaks BEP 15: a connect gets a connection id, an announce
// gets no peers if text is empty, else an error response with text.
func fakeUDPTracker(t *testing.T, text string) (string, *journal) {
	t.Helper()
	j := &journal{}
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			if n < 16 {
				continue
			}
			// Request header: connection id, action, transaction id.
			action := binary.BigEndian.Uint32(b[8:12])
			reply := binary.BigEndian.AppendUint32(nil, action)
			reply = append(reply, b[12:16]...)
			switch {
			case action == 0:
				reply = binary.BigEndian.AppendUint64(reply, 0x0123456789abcdef)
			case action == 1 && n >= 98:
				j.add(announced{
					event:    tracker.AnnounceEvent(binary.BigEndian.Uint32(b[80:84])).String(),
					numwant:  strconv.Itoa(int(int32(binary.BigEndian.Uint32(b[92:96])))),
					infoHash: string(b[16:36]),
					peerID:   string(b[36:56]),
					key:      strconv.Itoa(int(int32(binary.BigEndian.Uint32(b[88:92])))),
					port:     int(binary.BigEndian.Uint16(b[96:98])),
				})
				if text == "" {
					reply = append(reply, make([]byte, 12)...) // interval, leechers, seeders
				} else {
					binary.BigEndian.PutUint32(reply, 3)
					reply = append(reply, text...)
				}
			default:
				continue
			}
			_, _ = pc.WriteTo(reply, addr)
		}
	}()
	return "udp://" + pc.LocalAddr().String() + "/" + verifyKey + "/announce", j
}

// testServer allows the loopback fakes and shortens the timeouts. Its
// DEFAULT_TRACKERS is an accepting fake, which Verify must never reach.
func testServer(t *testing.T) (*server, *journal) {
	t.Helper()
	defaultFake, j := fakeHTTPTracker(t, "accept", "")
	s := newServer([]string{defaultFake})
	s.allowLocalNetworks = true
	s.verifyTimeout = 2 * time.Second
	s.verifyURLTimeout = 400 * time.Millisecond
	s.verifyStopTimeout = 400 * time.Millisecond
	return s, j
}

func verify(t *testing.T, s *server, ih string, urls ...string) *pb.VerifyReply {
	t.Helper()
	reply, err := s.Verify(context.Background(), &pb.VerifyRequest{InfoHash: ih, Trackers: []*pb.VerifyTier{{Urls: urls}}})
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func wantReply(t *testing.T, got *pb.VerifyReply, outcome, class string) {
	t.Helper()
	if got.GetOutcome() != outcome || got.GetClass() != class {
		t.Fatalf("reply {%q, %q}, want {%q, %q}", got.GetOutcome(), got.GetClass(), outcome, class)
	}
}

func verifyCount(t *testing.T, outcome, class string) float64 {
	t.Helper()
	return counterValue(t, "m2t_verify_total", map[string]string{"outcome": outcome, "class": class})
}

func TestVerifyAccepted(t *testing.T) {
	s, _ := testServer(t)
	ih, _ := hex.DecodeString(verifyIH)
	for _, c := range []struct {
		name string
		fake func(t *testing.T) (string, *journal)
	}{
		{"http", func(t *testing.T) (string, *journal) { return fakeHTTPTracker(t, "accept", "") }},
		{"udp", func(t *testing.T) (string, *journal) { return fakeUDPTracker(t, "") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, j := c.fake(t)
			before := verifyCount(t, verifyAccepted, "")
			wantReply(t, verify(t, s, verifyIH, u), verifyAccepted, "")
			if e := j.events(); e != "started,stopped" {
				t.Fatalf("tracker got %q, want started,stopped", e)
			}
			got := j.all()
			for _, a := range got {
				if a.numwant != "0" {
					t.Errorf("%s: numwant %q, want 0", a.event, a.numwant)
				}
				if a.infoHash != string(ih) {
					t.Errorf("%s: info_hash %x, want %s", a.event, a.infoHash, verifyIH)
				}
				if len(a.peerID) != 20 || !strings.HasPrefix(a.peerID, version.DefaultBep20Prefix) {
					t.Errorf("%s: peer_id %q, want 20 bytes after %q", a.event, a.peerID, version.DefaultBep20Prefix)
				}
				if a.port < 49152 || a.port > 65535 {
					t.Errorf("%s: port %d, want 49152-65535", a.event, a.port)
				}
			}
			if got[0].peerID != got[1].peerID || got[0].key != got[1].key || got[0].port != got[1].port {
				t.Errorf("stopped is another peer: %+v, then %+v", got[0], got[1])
			}
			if d := verifyCount(t, verifyAccepted, "") - before; d != 1 {
				t.Errorf("m2t_verify_total{accepted,\"\"} +%v, want +1", d)
			}
		})
	}
}

func TestVerifyRefused(t *testing.T) {
	s, _ := testServer(t)

	t.Run("http", func(t *testing.T) {
		u, j := fakeHTTPTracker(t, "refuse", "Invalid passkey")
		before := verifyCount(t, verifyRefused, classKey)
		wantReply(t, verify(t, s, verifyIH, u), verifyRefused, classKey)
		if e := j.events(); e != "started" {
			t.Fatalf("tracker got %q, want started and no stopped", e)
		}
		if d := verifyCount(t, verifyRefused, classKey) - before; d != 1 {
			t.Errorf("m2t_verify_total{refused,key} +%v, want +1", d)
		}
	})

	t.Run("udp", func(t *testing.T) {
		u, j := fakeUDPTracker(t, "Unregistered torrent")
		wantReply(t, verify(t, s, verifyIH, u), verifyRefused, classUnregistered)
		if e := j.events(); e != "started" {
			t.Fatalf("tracker got %q, want started and no stopped", e)
		}
	})

	t.Run("refused, then accepted", func(t *testing.T) {
		r, _ := fakeHTTPTracker(t, "refuse", "Invalid passkey")
		a, j := fakeHTTPTracker(t, "accept", "")
		wantReply(t, verify(t, s, verifyIH, r, a), verifyAccepted, "")
		if e := j.events(); e != "started,stopped" {
			t.Fatalf("second tracker got %q, want started,stopped", e)
		}
	})

	t.Run("the first accepted ends it", func(t *testing.T) {
		a, _ := fakeHTTPTracker(t, "accept", "")
		b, j := fakeHTTPTracker(t, "accept", "")
		wantReply(t, verify(t, s, verifyIH, a, b), verifyAccepted, "")
		if e := j.events(); e != "" {
			t.Fatalf("second tracker got %q, want nothing", e)
		}
	})

	t.Run("the class is the first refusal's", func(t *testing.T) {
		k, _ := fakeHTTPTracker(t, "refuse", "Invalid passkey")
		u, _ := fakeUDPTracker(t, "Unregistered torrent")
		wantReply(t, verify(t, s, verifyIH, k, u), verifyRefused, classKey)
	})
}

func TestVerifyUnreachable(t *testing.T) {
	s, _ := testServer(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String() + "/announce"
	l.Close()
	notFound, _, _ := httpEndpoint(t) // 404, no failure reason
	for _, u := range []string{closed, notFound} {
		wantReply(t, verify(t, s, verifyIH, u), verifyUnreachable, "")
	}
}

// Verify announces to the given trackers only. Both a check that ends at an
// accepting tracker and one that goes through all of them.
func TestVerifyDefaultTrackersUntouched(t *testing.T) {
	s, defaults := testServer(t)
	a, _ := fakeHTTPTracker(t, "accept", "")
	r, _ := fakeHTTPTracker(t, "refuse", "Invalid passkey")
	wantReply(t, verify(t, s, verifyIH, a), verifyAccepted, "")
	wantReply(t, verify(t, s, verifyIH, r), verifyRefused, classKey)
	if e := defaults.events(); e != "" {
		t.Fatalf("DEFAULT_TRACKERS got %q, want nothing", e)
	}
}

func TestVerifyRefusesLocalNetworksByDefault(t *testing.T) {
	s, _ := testServer(t)
	s.allowLocalNetworks = false
	h, hj := fakeHTTPTracker(t, "accept", "")
	u, uj := fakeUDPTracker(t, "")
	for tr, j := range map[string]*journal{h: hj, u: uj} {
		wantReply(t, verify(t, s, verifyIH, tr), verifyUnreachable, "")
		if e := j.events(); e != "" {
			t.Errorf("loopback tracker got %q, want nothing", e)
		}
	}
}

// Review Focus 1: a tracker can quote the key back in its refusal, and the
// URL carries it. Neither may reach the reply or any log line, including
// the logging interceptor's.
func TestVerifyKeepsTrackerTextIn(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	s, _ := testServer(t)
	u, _ := fakeHTTPTracker(t, "refuse", "Invalid passkey "+verifyKey)
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	g := newGRPCServer(s)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reply, err := pb.NewMagnet2TorrentClient(conn).Verify(ctx, &pb.VerifyRequest{
		InfoHash: verifyIH, Trackers: []*pb.VerifyTier{{Urls: []string{u}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReply(t, reply, verifyRefused, classKey)
	// The interceptor logs after the reply is sent; GracefulStop waits for it.
	conn.Close()
	g.GracefulStop()

	finished, intercepted := 0, false
	for _, e := range hook.AllEntries() {
		line, err := e.String()
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{verifyKey, "Invalid passkey", p.Path} {
			if strings.Contains(line, secret) {
				t.Errorf("log entry contains %q: %s", secret, line)
			}
		}
		if e.Data["grpc.method"] == "Verify" {
			intercepted = true
		}
		if e.Message == "verify finished" {
			finished++
			want := log.Fields{"infohash": verifyIH, "hosts": "127.0.0.1", "urls": 1, "outcome": verifyRefused, "class": classKey}
			for k, v := range want {
				if e.Data[k] != v {
					t.Errorf("verify finished: %s = %v, want %v", k, e.Data[k], v)
				}
			}
		}
	}
	if finished != 1 || !intercepted {
		t.Fatalf("%d verify finished entries and interceptor line %v; want 1 and true", finished, intercepted)
	}
}

// Review Focus 2: a tracker that never answers costs at most the per-URL
// timeout, and the whole check at most the overall one.
func TestVerifyHangingTrackerDoesNotBlockNext(t *testing.T) {
	s, _ := testServer(t)
	h, _ := fakeHTTPTracker(t, "hang", "")
	a, _ := fakeHTTPTracker(t, "accept", "")
	start := time.Now()
	wantReply(t, verify(t, s, verifyIH, h, a), verifyAccepted, "")
	if d := time.Since(start); d >= time.Second {
		t.Errorf("hanging tracker, then accepting: %v, want < 1s", d)
	}

	var hangs []string
	for range 5 {
		h, _ := fakeHTTPTracker(t, "hang", "")
		hangs = append(hangs, h)
	}
	start = time.Now()
	wantReply(t, verify(t, s, verifyIH, hangs...), verifyUnreachable, "")
	if d := time.Since(start); d > 2200*time.Millisecond {
		t.Errorf("five hanging trackers: %v, want <= 2.2s", d)
	}
}

// Review Focus 3: once a started was accepted, the stopped goes out even if
// the caller has gone, or the session stays open on the user's account.
func TestSendStoppedOutlivesCaller(t *testing.T) {
	s, _ := testServer(t)
	u, j := fakeHTTPTracker(t, "accept", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.sendStopped(ctx, u, tracker.AnnounceRequest{Event: tracker.Started, Port: 50000})
	if e := j.events(); e != "stopped" {
		t.Fatalf("tracker got %q, want stopped", e)
	}
}

// Review Focus 4: garbage in tr= is skipped, duplicates are announced once,
// and nothing usable means invalid without a single announce.
func TestVerifyURLSelection(t *testing.T) {
	s, _ := testServer(t)
	a, ja := fakeHTTPTracker(t, "accept", "")
	b, jb := fakeHTTPTracker(t, "accept", "")
	got := verifyURLs([]*pb.VerifyTier{
		{Urls: []string{a, "", "wss://x.invalid/a"}},
		{Urls: []string{a, b, "::bad", "udp:///announce"}},
	})
	if !slices.Equal(got, []string{a, b}) {
		t.Fatalf("verifyURLs = %q, want [A B]", got)
	}

	wantReply(t, verify(t, s, "xyz", a, b), verifyInvalid, "")
	wantReply(t, verify(t, s, strings.Repeat("ab", 19), a, b), verifyInvalid, "")
	wantReply(t, verify(t, s, verifyIH, "wss://x.invalid/a"), verifyInvalid, "")
	if e := ja.events() + jb.events(); e != "" {
		t.Fatalf("invalid requests reached the trackers: %q", e)
	}

	wantReply(t, verify(t, s, strings.ToUpper(verifyIH), a, a), verifyAccepted, "")
	if e := ja.events(); e != "started,stopped" {
		t.Fatalf("tracker named twice got %q, want started,stopped once", e)
	}
}

// The log names a tracker by its registrable domain: a subdomain can carry
// the key. A host without one is left out.
func TestVerifyHosts(t *testing.T) {
	got := verifyHosts([]string{
		"http://" + verifyKey + ".tracker.example/announce",
		"udp://Tracker.Example:6969/announce",
		"https://[2001:db8::1]:443/" + verifyKey + "/announce",
		"http://localhost/announce",
	})
	if got != "tracker.example,2001:db8::1" {
		t.Fatalf("verifyHosts = %q, want tracker.example,2001:db8::1", got)
	}
}
