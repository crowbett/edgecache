package rfc9111

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeadMatches applies RFC 9111 §4.3.5 to a 200 response to a HEAD request and
// one stored GET response that could have been chosen for it: the stored
// response may be updated from the HEAD's header fields only when every
// validator the HEAD carries (ETag, Last-Modified) matches the stored one and,
// if the HEAD has a Content-Length, it equals the stored body size. Otherwise
// the stored response is to be considered stale.
//
// A HEAD with no validator at all cannot show that the representation is the
// same one, so it never matches: the conservative reading of "matching values
// for any received validator fields".
func HeadMatches(stored http.Header, storedSize int64, head http.Header) bool {
	etag, lm := strings.TrimSpace(head.Get("ETag")), strings.TrimSpace(head.Get("Last-Modified"))
	if etag == "" && lm == "" {
		return false
	}
	if etag != "" && etag != strings.TrimSpace(stored.Get("ETag")) {
		return false
	}
	if lm != "" && !sameDate(lm, stored.Get("Last-Modified")) {
		return false
	}
	if cl := strings.TrimSpace(head.Get("Content-Length")); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n != storedSize {
			return false
		}
	}
	return true
}

// sameDate compares two HTTP-date field values: equal strings, or both parse
// to the same instant (one origin may switch between the RFC 9110 §5.6.7 forms).
func sameDate(a, b string) bool {
	b = strings.TrimSpace(b)
	if a == b {
		return true
	}
	ta, ok1 := ParseDate(a, time.Time{})
	tb, ok2 := ParseDate(b, time.Time{})
	return ok1 && ok2 && ta.Equal(tb)
}
