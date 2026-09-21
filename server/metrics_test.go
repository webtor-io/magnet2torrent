package main

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestResolveOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, outcomeOK},
		{"10-minute cap", errResolveTooLong, outcomeTimeout},
		{"caller deadline (raw ctx error)", context.DeadlineExceeded, outcomeTimeout},
		{"caller deadline (status)", status.Error(codes.DeadlineExceeded, "x"), outcomeTimeout},
		{"caller went away (raw ctx error)", context.Canceled, outcomeCanceled},
		{"caller went away (status)", status.Error(codes.Canceled, "x"), outcomeCanceled},
		{"recovered panic", status.Error(codes.Internal, "internal error: boom"), outcomeError},
		{"malformed magnet", errors.New("magnet2torrent: bad magnet"), outcomeError},
		// Some other Aborted must not be mistaken for the cap.
		{"foreign Aborted", status.Error(codes.Aborted, "other"), outcomeError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveOutcome(c.err); got != c.want {
				t.Fatalf("resolveOutcome(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestGRPCServerMetrics drives one RPC through the production interceptor
// chain over bufconn and checks the standard and the service counters.
func TestGRPCServerMetrics(t *testing.T) {
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

	handled := map[string]string{"grpc_service": "Magnet2Torrent", "grpc_method": "Magnet2Torrent", "grpc_code": "Unknown"}
	resolved := map[string]string{"outcome": outcomeError}
	handledBefore := counterValue(t, "grpc_server_handled_total", handled)
	resolvedBefore := counterValue(t, "m2t_resolve_total", resolved)

	// A string that is not a magnet fails in AddMagnet before any network
	// activity and comes back as a plain error, i.e. codes.Unknown.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = pb.NewMagnet2TorrentClient(conn).Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: "not a magnet"})
	if status.Code(err) != codes.Unknown {
		t.Fatalf("want codes.Unknown, got %v", err)
	}

	if got := counterValue(t, "grpc_server_handled_total", handled) - handledBefore; got != 1 {
		t.Fatalf("grpc_server_handled_total%v: got +%v, want +1", handled, got)
	}
	if got := counterValue(t, "m2t_resolve_total", resolved) - resolvedBefore; got != 1 {
		t.Fatalf("m2t_resolve_total%v: got +%v, want +1", resolved, got)
	}
	if got := gaugeValue(t, "m2t_resolve_in_flight"); got != 0 {
		t.Fatalf("m2t_resolve_in_flight: got %v after the call, want 0", got)
	}
	// InitializeMetrics: the OK series exists (at 0) although no call succeeded.
	handled["grpc_code"] = "OK"
	if got := counterValue(t, "grpc_server_handled_total", handled); got != 0 {
		t.Fatalf("grpc_server_handled_total%v: got %v, want pre-initialised 0", handled, got)
	}
}

// counterValue reads one series of a counter vec from the default registry.
// A series that does not exist reads as 0 — a missing series is what
// InitializeMetrics is there to prevent, and the OK check above relies on
// telling "present at 0" from "absent", hence the explicit fatal.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, name, labels)
	if m == nil {
		t.Fatalf("no series %s%v", name, labels)
	}
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	m := findMetric(t, name, nil)
	if m == nil {
		t.Fatalf("no series %s", name)
	}
	return m.GetGauge().GetValue()
}

func findMetric(t *testing.T, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			for k, v := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k {
						if lp.GetValue() != v {
							continue next
						}
						found = true
					}
				}
				if !found {
					continue next
				}
			}
			return m
		}
	}
	return nil
}
