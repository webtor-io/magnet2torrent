package main

import (
	"net"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type bigReplyServer struct {
	pb.UnimplementedMagnet2TorrentServer
}

func (bigReplyServer) Magnet2Torrent(context.Context, *pb.Magnet2TorrentRequest) (*pb.Magnet2TorrentReply, error) {
	return &pb.Magnet2TorrentReply{Torrent: make([]byte, 2<<20)}, nil
}

// TestReplyNotLogged guards against payload logging in the interceptor chain:
// a multi-megabyte .torrent in one log line is rejected by Loki and stalls
// promtail for the whole node. Lives in a file sorted after metrics_test.go:
// its successful call moves the shared OK counter that TestGRPCServerMetrics
// expects at 0.
func TestReplyNotLogged(t *testing.T) {
	hook := logtest.NewGlobal()
	lis := bufconn.Listen(1 << 20)
	s := newGRPCServer(bigReplyServer{})
	go func() { _ = s.Serve(lis) }()
	defer s.Stop()

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
	if _, err := pb.NewMagnet2TorrentClient(conn).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: "x"}); err != nil {
		t.Fatal(err)
	}

	if len(hook.AllEntries()) == 0 {
		t.Fatal("no log entries captured; the check below would pass vacuously")
	}
	for _, e := range hook.AllEntries() {
		line, err := e.String()
		if err != nil {
			t.Fatal(err)
		}
		if len(line) > 64<<10 {
			t.Fatalf("log entry %q is %d bytes", e.Message, len(line))
		}
	}
}
