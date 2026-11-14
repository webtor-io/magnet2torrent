package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
)

// TestAnnounceFailureLogRedacted: the torrent fork logs a failed announce
// itself. The service sets no Logger on the client, so the line goes through
// anacrolix/log's default handler straight to stderr, beside logrus's lines,
// and is shipped with them. A private tracker's key, in the announce URL's
// path or query or quoted back in the tracker's refusal, must not be in it;
// the fork redacts it (webtor-io/torrent#8), this checks the pinned one does.
//
// That handler holds the stderr it saw at init, so the request runs in a
// child process and the test reads everything the child wrote.
func TestAnnounceFailureLogRedacted(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	if os.Getenv("M2T_ANNOUNCE_LOG_CHILD") == "1" {
		announceToRefusingTracker(key)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAnnounceFailureLogRedacted$")
	cmd.Env = append(os.Environ(), "M2T_ANNOUNCE_LOG_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "announce failed") {
		t.Fatalf("no announce failure line from the fork; the check below would pass vacuously:\n%s", out)
	}
	// A dial refusal also logs "announce failed"; only the tracker's own
	// refusal carries the quoted key.
	if !strings.Contains(string(out), "Invalid passkey") {
		t.Fatalf("the tracker's refusal is not in the output, it was never reached:\n%s", out)
	}
	if strings.Contains(string(out), key) {
		t.Fatalf("output contains the tracker key:\n%s", out)
	}
}

// announceToRefusingTracker resolves a fake magnet whose only tracker, on
// 127.0.0.1, refuses the key and quotes it back, as trackers commonly do.
func announceToRefusingTracker(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reason := "Invalid passkey (33 - " + key + ")"
		fmt.Fprintf(w, "d14:failure reason%d:%se", len(reason), reason)
		// The fork logs a failure at debug level once the request is over;
		// a second leaves it time to log this one.
		time.AfterFunc(time.Second, cancel)
	}))
	defer tr.Close()
	s := newServer(nil)
	s.allowLocalNetworks = true // the tracker is on loopback
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20) +
		"&tr=" + url.QueryEscape(tr.URL+"/"+key+"/announce?passkey="+key)
	_, _ = s.Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: magnet})
}
