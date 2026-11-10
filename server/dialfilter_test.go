package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
)

func TestIsLocalNetworkAddr(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.255.255.254", "::1",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.0.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fe80::1%eth0",
		"0.0.0.0", "::", "::%eth0",
		"224.0.0.1", "239.255.255.250", "ff02::1",
		"100.64.0.0", "100.127.255.255",
		"fc00::1", "fd12:3456::1",
		// IPv4-mapped forms reach the IPv4 address.
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"::ffff:0.0.0.0", "::ffff:100.64.0.1", "::ffff:224.0.0.1",
	}
	allowed := []string{
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1",
		// Just outside the refused ranges.
		"100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0",
		"169.253.255.255", "126.255.255.255", "::ffff:192.0.2.1",
	}
	for _, s := range refused {
		if !isLocalNetworkAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s: allowed, want refused", s)
		}
	}
	for _, s := range allowed {
		if isLocalNetworkAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s: refused, want allowed", s)
		}
	}
	// An unset address means this host to the kernel.
	if !isLocalNetworkAddr(netip.Addr{}) {
		t.Error("zero Addr: allowed, want refused")
	}
}

// recordingPacketConn stands in for the UDP socket, so that the allowed case
// sends nothing anywhere.
type recordingPacketConn struct {
	net.PacketConn
	writes int
}

func (c *recordingPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.writes++
	return len(p), nil
}

// TestLocalNetworkHooks: both hooks refuse what they cannot read, and the UDP
// one refuses before the socket sees the packet.
func TestLocalNetworkHooks(t *testing.T) {
	for addr, refused := range map[string]bool{
		"192.0.2.1:80":          false,
		"[2001:db8::1]:80":      false,
		"127.0.0.1:80":          true,
		"[::ffff:127.0.0.1]:80": true,
		"[fe80::1%eth0]:80":     true,
		"169.254.169.254:80":    true,
		"not an address":        true,
		"tracker.invalid:80":    true, // the hook only ever sees resolved addresses
	} {
		err := refuseLocalNetworks("tcp4", addr, nil)
		if refused != errors.Is(err, errLocalNetwork) {
			t.Errorf("Control(%q) = %v, want refused=%v", addr, err, refused)
		}
	}

	for _, c := range []struct {
		addr    net.Addr
		refused bool
	}{
		{&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 80}, false},
		{&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}, true},
		{&net.UDPAddr{IP: net.ParseIP("::ffff:10.0.0.1"), Port: 80}, true},
		{&net.UDPAddr{IP: net.IPv4zero, Port: 80}, true},
		{&net.UDPAddr{Port: 80}, true},                    // no IP: sent to this host
		{&net.IPAddr{IP: net.ParseIP("192.0.2.1")}, true}, // not a UDP address
		{(*net.UDPAddr)(nil), true},
	} {
		rec := &recordingPacketConn{}
		_, err := localNetworkRefusingConn{rec}.WriteTo([]byte("x"), c.addr)
		if c.refused != errors.Is(err, errLocalNetwork) || c.refused != (rec.writes == 0) {
			t.Errorf("WriteTo(%v) = %v with %d writes, want refused=%v", c.addr, err, rec.writes, c.refused)
		}
	}
}

// TestTrackerLocalNetworks drives the handler against a tracker on loopback:
// refused by default, with one warning naming the flag, and reached once the
// flag allows local networks. The allowed run shows that the refused one
// would have connected, so its "nothing arrived" is not vacuous.
func TestTrackerLocalNetworks(t *testing.T) {
	for _, c := range []struct {
		name    string
		tracker func(t *testing.T) (trackerURL, addr string, hits *atomic.Int64)
		// Announced through DEFAULT_TRACKERS instead of the magnet's tr=.
		viaDefault bool
	}{
		{name: "http tr=", tracker: httpTracker},
		{name: "udp tr=", tracker: udpTracker},
		{name: "http default", tracker: httpTracker, viaDefault: true},
	} {
		for _, allow := range []bool{false, true} {
			name := c.name + " refused"
			if allow {
				name = c.name + " allowed"
			}
			t.Run(name, func(t *testing.T) {
				hook := logtest.NewGlobal()
				defer hook.Reset()
				trackerURL, addr, hits := c.tracker(t)
				magnet := "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20)
				var defaults []string
				if c.viaDefault {
					defaults = []string{trackerURL}
				} else {
					magnet += "&tr=" + url.QueryEscape(trackerURL)
				}
				s := newServer(defaults)
				s.allowLocalNetworks = allow

				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = s.Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: magnet})
				}()
				defer func() { cancel(); <-done }()

				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if n := hits.Load(); n > 0 {
						if allow {
							return
						}
						t.Fatalf("tracker got %d connections or packets, want none", n)
					}
					if !allow && refusalLogged(hook, addr) {
						// The refusal comes before the connect or the send.
						time.Sleep(100 * time.Millisecond)
						if n := hits.Load(); n != 0 {
							t.Fatalf("refusal logged, yet the tracker got %d connections or packets", n)
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				if allow {
					t.Fatal("tracker never reached with local networks allowed")
				}
				t.Fatalf("no refusal logged for %s; tracker got %d", addr, hits.Load())
			})
		}
	}
}

// refusalLogged: a warning for exactly this address that names the flag.
func refusalLogged(hook *logtest.Hook, addr string) bool {
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && e.Data["addr"] == addr && strings.Contains(e.Message, "--"+allowLocalNetworksFlag) {
			return true
		}
	}
	return false
}

// httpTracker counts TCP connections, not requests: a connection that never
// sends one is already what the filter must prevent.
func httpTracker(t *testing.T) (string, string, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "test tracker", http.StatusNotFound)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			hits.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL + "/announce", srv.Listener.Addr().String(), &hits
}

func udpTracker(t *testing.T) (string, string, *atomic.Int64) {
	var hits atomic.Int64
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(b); err != nil {
				return
			}
			hits.Add(1)
		}
	}()
	return "udp://" + pc.LocalAddr().String() + "/announce", pc.LocalAddr().String(), &hits
}
