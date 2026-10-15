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
