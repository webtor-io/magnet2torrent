package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent/tracker"
	"github.com/anacrolix/torrent/version"
	log "github.com/sirupsen/logrus"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/publicsuffix"
)

// Verify outcomes, in the reply and in m2t_verify_total.
const (
	verifyAccepted    = "accepted"
	verifyRefused     = "refused"
	verifyUnreachable = "unreachable"
	verifyInvalid     = "invalid"
)

// verifyLogger discards the tracker library's log, whose lines quote the
// tracker URL and so the key. A zero Logger would not do: the UDP client
// replaces it with the default one, which writes to stderr.
var verifyLogger = func() alog.Logger {
	var l alog.Logger
	l.SetHandlers(alog.DiscardHandler)
	return l
}()

// Verify checks that a tracker accepts the key in its URL for a known
// info-hash. One URL at a time it sends one started with numwant=0, and to the
// first that accepts a stopped, which ends the check. No torrent client: no
// DHT, no PEX, no DEFAULT_TRACKERS. Only the outcome and the class of the
// first refusal leave the call; the tracker's text and the URL can carry the
// key, so neither is logged nor returned. The error is always nil (a panic
// becomes Internal in grpc_recovery).
func (s *server) Verify(ctx context.Context, in *pb.VerifyRequest) (*pb.VerifyReply, error) {
	urls := verifyURLs(in.GetTrackers())
	outcome, class, ih := verifyInvalid, "", ""
	b, err := hex.DecodeString(in.GetInfoHash())
	if err == nil && len(b) == 20 {
		ih = hex.EncodeToString(b)
	}
	if ih != "" && len(urls) > 0 {
		outcome = verifyUnreachable
		req := tracker.AnnounceRequest{
			InfoHash: [20]byte(b),
			Left:     -1, // unknown; HTTP sends 2^63-1
			Event:    tracker.Started,
			Key:      rand.Int32(),
			NumWant:  0,
			Port:     uint16(49152 + rand.IntN(65536-49152)),
		}
		copy(req.PeerId[:], version.DefaultBep20Prefix)
		_, _ = crand.Read(req.PeerId[len(version.DefaultBep20Prefix):])

		all, cancel := context.WithTimeout(ctx, s.verifyTimeout)
		defer cancel()
		for _, u := range urls {
			if all.Err() != nil {
				break
			}
			one, cancelOne := context.WithTimeout(all, s.verifyURLTimeout)
			err := s.announce(one, u, req)
			cancelOne()
			if err == nil {
				s.sendStopped(ctx, u, req)
				outcome, class = verifyAccepted, ""
				break
			}
			if text, ok := refusalText(err); ok && outcome != verifyRefused {
				outcome, class = verifyRefused, classifyRefusal(text)
			}
		}
	}
	verifyTotal.WithLabelValues(outcome, class).Inc()
	log.WithFields(log.Fields{
		"infohash": ih,
		"hosts":    verifyHosts(urls),
		"urls":     len(urls),
		"outcome":  outcome,
		"class":    class,
	}).Info("verify finished")
	return &pb.VerifyReply{Outcome: outcome, Class: class}, nil
}

// verifyURLs flattens the tiers in order and keeps each http, https and udp
// URL with a host once; nothing else can be announced to.
func verifyURLs(tiers []*pb.VerifyTier) []string {
	var out []string
	for _, tier := range tiers {
		for _, u := range tier.GetUrls() {
			p, err := url.Parse(u)
			if err != nil || p.Hostname() == "" || !slices.Contains([]string{"http", "https", "udp"}, p.Scheme) || slices.Contains(out, u) {
				continue
			}
			out = append(out, u)
		}
	}
	return out
}

// verifyHosts names the trackers for the log by their registrable domain,
// which drops a subdomain that could carry a key, or by their IP address.
// A host with no registrable domain is left out.
func verifyHosts(urls []string) string {
	var hosts []string
	for _, u := range urls {
		p, _ := url.Parse(u) // verifyURLs kept only those that parse
		h := strings.ToLower(p.Hostname())
		if _, err := netip.ParseAddr(h); err != nil {
			if h, err = publicsuffix.EffectiveTLDPlusOne(h); err != nil {
				continue
			}
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return strings.Join(hosts, ",")
}

// announce sends one announce through the local network filter and keeps
// only whether the tracker accepted it.
func (s *server) announce(ctx context.Context, u string, req tracker.AnnounceRequest) error {
	dial, listen := s.trackerDialers()
	_, err := tracker.Announce{
		TrackerUrl: u,
		Request:    req,
		UserAgent:  version.DefaultHttpUserAgent,
		// Pods have no IPv6 route (see DisableIPv6 in newClient).
		UdpNetwork:   "udp4",
		Context:      ctx,
		DialContext:  dial,
		ListenPacket: listen,
		// The HTTP announcer sends no numwant at all, and a tracker's default
		// is a list of peers; UDP sends req.NumWant.
		HttpRequestDirector: func(r *http.Request) error {
			r.URL.RawQuery += "&numwant=0"
			return nil
		},
		Logger: verifyLogger,
	}.Do()
	return err
}

// sendStopped closes the session an accepted started opened on the tracker,
// as the same peer (peer_id, key, port). Its context is cut off from the
// caller's: a caller that has gone would otherwise leave the session open on
// the user's account. The result is not logged: its error quotes the URL.
func (s *server) sendStopped(ctx context.Context, u string, req tracker.AnnounceRequest) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.verifyStopTimeout)
	defer cancel()
	req.Event = tracker.Stopped
	_ = s.announce(ctx, u, req)
}
