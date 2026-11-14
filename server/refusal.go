package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent/tracker/udp"
)

// The class of a tracker's refusal is all that leaves the process: the
// tracker's own text can quote the key back.
const (
	classKey          = "key"
	classAccount      = "account"
	classUnregistered = "unregistered"
	classClient       = "client"
	classOther        = "other"
)

// refusalClasses are tried in order: "torrent not found for this passkey" is
// about the torrent, "banned client" about the client.
var refusalClasses = []struct {
	class string
	re    *regexp.Regexp
}{
	{classUnregistered, regexp.MustCompile(`(?i)unregistered|not registered|torrent not found|unknown torrent|info_?hash not found|torrent does not exist`)},
	{classKey, regexp.MustCompile(`(?i)pass ?key|auth ?key|torrent ?pass|user not found|unknown user|invalid key|invalid user`)},
	{classClient, regexp.MustCompile(`(?i)client|white ?list|black ?list|\bip\b`)},
	{classAccount, regexp.MustCompile(`(?i)account|ratio|limit|disabled|suspended|banned|warned|not allowed to download|leech|slots?`)},
}

func classifyRefusal(text string) string {
	for _, c := range refusalClasses {
		if c.re.MatchString(text) {
			return c.class
		}
	}
	return classOther
}

// The HTTP announcer's error for a reply with a failure reason, followed by
// the reason, quoted.
const failureReasonPrefix = "tracker gave failure reason: "

// refusalText returns the tracker's text if err is a refusal: an HTTP failure
// reason or a UDP error response. A status other than 200, an unreadable
// reply, the network, a timeout are not, nor is a UDP connection ID
// mismatch: that is the protocol's, not the tracker's word on the key.
func refusalText(err error) (string, bool) {
	var er udp.ErrorResponse
	if errors.As(err, &er) {
		if er.Message == udp.ConnectionIdMissmatchNul {
			return "", false
		}
		return er.Message, true
	}
	for ; err != nil; err = errors.Unwrap(err) {
		if q, ok := strings.CutPrefix(err.Error(), failureReasonPrefix); ok {
			if text, uerr := strconv.Unquote(q); uerr == nil {
				return text, true
			}
			return q, true
		}
	}
	return "", false
}
