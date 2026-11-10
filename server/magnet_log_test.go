package main

import (
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestMagnetNotLogged: a magnet's tr= can carry a private tracker's passkey,
// in the query or in the path, and log lines end up in shared log storage.
// No line logged during the call, and no error returned to the caller (the
// logging interceptor logs it too), may quote the magnet or its trackers.
func TestMagnetNotLogged(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	const host = "tracker.invalid"
	ih := strings.Repeat("ab", 20)
	tr := url.QueryEscape("http://" + host + "/" + key + "/announce?passkey=" + key)
	cases := []struct {
		name, magnet string
		// The entry that shows the request was logged at all, so the
		// absence checks below cannot pass vacuously.
		wantMsg, wantField string
		wantValue          any
	}{
		{
			name:    "valid",
			magnet:  "magnet:?xt=urn:btih:" + ih + "&tr=" + tr,
			wantMsg: "processing new request", wantField: "infoHash", wantValue: ih,
		},
		{
			// url.Parse rejects a control character and quotes the whole
			// input in its error.
			name:    "unparsable",
			magnet:  "magnet:?xt=urn:btih:" + ih + "&tr=" + tr + "\n",
			wantMsg: "failed parsing magnet", wantField: "len",
		},
		{
			// An escaped separator puts the tracker inside xt=, and the
			// infohash parse error quotes the whole xt value.
			name:    "tracker inside xt",
			magnet:  "magnet:?xt=urn:btih:" + ih + "%26tr=" + tr,
			wantMsg: "failed parsing magnet", wantField: "len",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.wantField == "len" {
				c.wantValue = len(c.magnet)
			}
			hook := logtest.NewGlobal()
			defer hook.Reset()
			lis := bufconn.Listen(1 << 20)
			s := newGRPCServer(newServer(nil))
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

			// The valid magnet would be resolved for real; the deadline ends
			// it once the request has been logged.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err = pb.NewMagnet2TorrentClient(conn).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: c.magnet})
			if err == nil {
				t.Fatal("call succeeded; want an error")
			}
			msg := status.Convert(err).Message()
			for _, secret := range []string{key, host} {
				if strings.Contains(msg, secret) {
					t.Errorf("error returned to the caller contains %q: %s", secret, msg)
				}
			}
			// The client gives up at its deadline while the handler may still
			// run; GracefulStop waits for it and so for the interceptor's line.
			conn.Close()
			s.GracefulStop()

			found := false
			for _, e := range hook.AllEntries() {
				line, err := e.String()
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{key, host} {
					if strings.Contains(line, secret) {
						t.Errorf("log entry contains %q: %s", secret, line)
					}
				}
				if e.Message == c.wantMsg && e.Data[c.wantField] == c.wantValue {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %q entry with %s=%v; the checks above may be vacuous", c.wantMsg, c.wantField, c.wantValue)
			}
		})
	}
}
