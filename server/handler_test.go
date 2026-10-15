package main

import (
	"strings"
	"testing"

	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A caller that gave up must come back as Canceled, not as a bare context
// error, which status.Code (and so the logging interceptor) reads as Unknown.
func TestCallerGaveUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newServer(nil).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)})
	if c := status.Code(err); c != codes.Canceled {
		t.Fatalf("code %v (%v), want Canceled", c, err)
	}
	if o := resolveOutcome(err); o != outcomeCanceled {
		t.Fatalf("outcome %q, want %q", o, outcomeCanceled)
	}
}

// The client panics on a zero v1 info-hash; such a magnet is the caller's
// mistake, not an Internal error.
func TestZeroInfoHash(t *testing.T) {
	for _, magnet := range []string{
		"magnet:?xt=urn:btih:" + strings.Repeat("0", 40),
		"magnet:?dn=x",
	} {
		_, err := newServer(nil).Magnet2Torrent(context.Background(), &pb.Magnet2TorrentRequest{Magnet: magnet})
		if c := status.Code(err); c != codes.InvalidArgument {
			t.Errorf("%s: code %v (%v), want InvalidArgument", magnet, c, err)
		}
	}
}

// Pods have no IPv6 route: a udp:// tracker must not get a udp6 announcer,
// whose every announce fails with "network is unreachable".
func TestNoIPv6Announcer(t *testing.T) {
	cl, err := newServer(nil).newClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.AddMagnet("magnet:?xt=urn:btih:" + strings.Repeat("ab", 20) + "&tr=udp://127.0.0.1:1/announce"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	cl.WriteStatus(&b)
	if !strings.Contains(b.String(), "udp4://127.0.0.1:1") {
		t.Fatalf("no udp4 announcer; the check below would pass vacuously:\n%s", b.String())
	}
	if strings.Contains(b.String(), "udp6://") {
		t.Fatalf("udp6 announcer started:\n%s", b.String())
	}
}
