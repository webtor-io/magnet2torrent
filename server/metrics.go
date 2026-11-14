package main

import (
	"errors"
	"time"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcMetrics feeds the standard grpc_server_* series (started/handled/msg
// counters and the handling-time histogram). The histogram reaches 120 s
// because a resolve legitimately runs for the caller's whole 60 s deadline
// and web-ui's retries can push a single call past that; the default
// buckets top out at 10 s and would put every real resolve in +Inf.
var grpcMetrics = grpcprom.NewServerMetrics(
	grpcprom.WithServerHandlingTimeHistogram(
		grpcprom.WithHistogramBuckets([]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}),
	),
)

// Resolve outcomes. The gRPC code alone is not enough: the 10-minute cap
// returns Aborted and the caller's deadline returns DeadlineExceeded, yet for
// the operator both are the same thing — nobody in the swarm answered — and
// that is the expected fate of a dead magnet (33-38% of them in prod), not a
// service fault. Canceled is not a fault either and must not inflate the
// error rate. In practice it is the same dead magnet seen from the other
// side: web-ui gives up after 60 s and cancels before any deadline fires
// (2 days to 2026-10-15: 47% canceled, 0 timeout; 88% of the canceled ones
// ended between 30 and 60 s). Invalid is the caller's mistake (a zero
// info-hash), not a fault either; in those 2 days it was all of error (3).
// Private refused is a private torrent's magnet without trackers of its own:
// resolved fine, but there is nothing it may be announced to.
const (
	outcomeOK             = "ok"
	outcomeTimeout        = "timeout"
	outcomeCanceled       = "canceled"
	outcomeInvalid        = "invalid"
	outcomePrivateRefused = "private_refused"
	outcomeError          = "error"
)

// errResolveTooLong is the 10-minute cap. It is a package variable so the
// outcome mapping can recognise it by identity instead of by the Aborted code,
// which nothing else in this server produces today but could tomorrow.
var errResolveTooLong = status.Error(codes.Aborted, "fetching torrent takes too long")

// errPrivateMagnet refuses a private torrent (BEP 27) whose magnet has no tr=:
// it may be announced only to its own trackers. The caller tells it from other
// failures by the ErrorInfo reason, the outcome mapping by identity.
var errPrivateMagnet = func() error {
	st, err := status.New(codes.FailedPrecondition, "private torrent: a magnet without trackers of its own cannot be resolved").
		WithDetails(&errdetails.ErrorInfo{Domain: "magnet2torrent", Reason: "private_magnet"})
	if err != nil {
		panic(err)
	}
	return st.Err()
}()

var (
	resolveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "m2t_resolve_total",
		Help: "Magnet resolutions finished, by outcome. timeout = nobody in the swarm answered before the deadline (dead magnet), canceled = the caller (web-ui, 60 s) gave up waiting, mostly dead magnets too, not a service failure, invalid = the caller sent an unusable magnet (zero info-hash), private_refused = a private torrent's magnet without trackers of its own, error = the service itself failed.",
	}, []string{"outcome"})

	// Labelled by outcome so that the latency of successful resolves is
	// queryable on its own: timeouts sit at exactly 60 s / 600 s and would
	// otherwise dominate the upper buckets.
	resolveSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "m2t_resolve_seconds",
		Help:    "Wall time of a magnet resolution from request to metainfo (or failure), by outcome.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 90, 120, 180},
	}, []string{"outcome"})

	resolveInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "m2t_resolve_in_flight",
		Help: "Magnet resolutions currently running; each holds its own torrent client and DHT node.",
	})

	// No label of the caller's: the class is all a refusal leaves behind.
	verifyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "m2t_verify_total",
		Help: "Verify calls (does a tracker accept the key in its URL for a known info-hash), by outcome and, when refused, the class of the first refusal. accepted = a tracker accepted, refused = none accepted and one refused, unreachable = none accepted or refused (network, timeout, an HTTP status other than 200, a local network address), invalid = no usable info-hash or tracker URL, nothing sent. A call whose caller left before any tracker accepted or refused is not counted.",
	}, []string{"outcome", "class"})

	// The warning is logged once a minute; this carries the volume.
	localNetworkRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "m2t_local_network_refusals_total",
		Help: "Tracker and metainfo source connections, and UDP tracker sends, refused because the address is on a local network (ALLOW_LOCAL_NETWORKS).",
	})
)

func init() {
	prometheus.MustRegister(grpcMetrics, resolveTotal, resolveSeconds, resolveInFlight, verifyTotal, localNetworkRefusals)
	// The label set is closed, so every series can exist from the start:
	// a dashboard then shows 0 rather than "no data", and rate() over a
	// counter that appears mid-window does not miss its first increment.
	for _, o := range []string{outcomeOK, outcomeTimeout, outcomeCanceled, outcomeInvalid, outcomePrivateRefused, outcomeError} {
		resolveTotal.WithLabelValues(o)
		resolveSeconds.WithLabelValues(o)
	}
	for _, o := range []string{verifyAccepted, verifyUnreachable, verifyInvalid} {
		verifyTotal.WithLabelValues(o, "")
	}
	for _, c := range []string{classKey, classAccount, classUnregistered, classClient, classOther} {
		verifyTotal.WithLabelValues(verifyRefused, c)
	}
}

// startResolve accounts one in-flight resolution and returns the function
// that finishes it. Callers must defer it BEFORE the panic-recovery defer, so
// that (defers running last-in-first-out) the recovery has already turned the
// panic into err by the time the outcome is recorded.
func startResolve() func(err error) {
	resolveInFlight.Inc()
	start := time.Now()
	return func(err error) {
		resolveInFlight.Dec()
		o := resolveOutcome(err)
		resolveTotal.WithLabelValues(o).Inc()
		resolveSeconds.WithLabelValues(o).Observe(time.Since(start).Seconds())
	}
}

// resolveOutcome maps the error a resolve finished with onto a bounded label.
func resolveOutcome(err error) string {
	switch {
	case err == nil:
		return outcomeOK
	case errors.Is(err, errPrivateMagnet):
		return outcomePrivateRefused
	case errors.Is(err, errResolveTooLong), errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout
	case errors.Is(err, context.Canceled):
		return outcomeCanceled
	}
	// Errors already shaped as a status (e.g. forwarded by an interceptor).
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return outcomeTimeout
	case codes.Canceled:
		return outcomeCanceled
	case codes.InvalidArgument:
		return outcomeInvalid
	}
	return outcomeError
}
