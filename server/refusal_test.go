package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/tracker/udp"
)

func TestClassifyRefusal(t *testing.T) {
	for text, want := range map[string]string{
		"Unregistered torrent":                     "unregistered",
		"Torrent not registered with this tracker": "unregistered",
		"Torrent not found for this passkey":       "unregistered",
		"Invalid passkey":                          "key",
		"passkey not found":                        "key",
		"User not found":                           "key",
		"Invalid passkey for this torrent":         "key",
		"Your client is not on the whitelist":      "client",
		"Banned client":                            "client",
		"Your IP is not allowed":                   "client",
		"Your account is disabled":                 "account",
		"Your ratio is too low":                    "account",
		"You have reached your download limit":     "account",
		"You are not allowed to download (warned)": "account",
		"Tracker is down for maintenance":          "other",
		"":                                         "other",
	} {
		if got := classifyRefusal(text); got != want {
			t.Errorf("classifyRefusal(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestRefusalText(t *testing.T) {
	httpRefusal := fmt.Errorf("tracker gave failure reason: %q", "Invalid passkey")
	for _, c := range []struct {
		name string
		err  error
		text string
		ok   bool
	}{
		{"http", httpRefusal, "Invalid passkey", true},
		{"http, wrapped", fmt.Errorf("announce: %w", httpRefusal), "Invalid passkey", true},
		{"http, quotes in the text", fmt.Errorf("tracker gave failure reason: %q", `say "no"`), `say "no"`, true},
		{"http, not quoted", errors.New("tracker gave failure reason: bare text"), "bare text", true},
		// The fork's format once announce errors wrap a sentinel (torrent#8).
		{"http, sentinel", fmt.Errorf("%w: %q", errors.New("tracker gave failure reason"), "Invalid passkey"), "Invalid passkey", true},
		{"udp", udp.ErrorResponse{Message: "Unregistered torrent"}, "Unregistered torrent", true},
		{"udp, wrapped", fmt.Errorf("announce: %w", udp.ErrorResponse{Message: "Unregistered torrent"}), "Unregistered torrent", true},
		{"http status", errors.New("response from tracker: 403 Forbidden: 0 bytes"), "", false},
		{"timeout", context.DeadlineExceeded, "", false},
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "", false},
		{"local network", errLocalNetwork, "", false},
	} {
		text, ok := refusalText(c.err)
		if text != c.text || ok != c.ok {
			t.Errorf("%s: refusalText(%v) = (%q, %v), want (%q, %v)", c.name, c.err, text, ok, c.text, c.ok)
		}
	}
}
