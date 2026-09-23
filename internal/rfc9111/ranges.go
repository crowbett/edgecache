package rfc9111

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Byte-range support for serving a stored full representation (RFC 9110 §14).
// The engine stores only complete 200 responses; a 206 is never stored
// (Storable refuses it, see §3.4 of RFC 9111), so every range served from
// cache is cut from a full body whose length is known.

// ByteRange is one satisfiable range resolved against a representation of a
// known length: bytes [Start, Start+Length).
type ByteRange struct {
	Start, Length int64
}

// End is the last-pos of r (inclusive), as Content-Range spells it.
func (r ByteRange) End() int64 { return r.Start + r.Length - 1 }

// ContentRange formats the Content-Range value for r within a representation
// of size bytes (RFC 9110 §14.4): "bytes first-last/complete-length".
func (r ByteRange) ContentRange(size int64) string {
	return "bytes " + strconv.FormatInt(r.Start, 10) + "-" + strconv.FormatInt(r.End(), 10) + "/" + strconv.FormatInt(size, 10)
}

// UnsatisfiedRange is the Content-Range of a 416 (RFC 9110 §14.4, §15.5.17).
func UnsatisfiedRange(size int64) string {
	return "bytes */" + strconv.FormatInt(size, 10)
}

// MaxRanges caps how many range-specs ParseRange accepts; beyond it the whole
// header is ignored (RFC 9110 §14.2 lets a server ignore a Range with "many
// small ranges", a known amplification vector).
const MaxRanges = 64

var (
	// ErrRangeIgnored means the Range header must be ignored and the full
	// representation served with 200: bad syntax, an unknown range unit, too
	// many ranges, or ranges that together ask for more bytes than the
	// representation has (RFC 9110 §14.2 permits ignoring all of these).
	ErrRangeIgnored = errors.New("rfc9111: range ignored")
	// ErrRangeUnsatisfiable means the header was valid but no range overlaps
	// the representation: answer 416 with Content-Range "bytes */size"
	// (RFC 9110 §14.1.2, §15.5.17).
	ErrRangeUnsatisfiable = errors.New("rfc9111: range not satisfiable")
)

// ParseRange parses a Range field value against a representation of size
// bytes (RFC 9110 §14.1.2, §14.2):
//
//	Range          = ranges-specifier
//	ranges-specifier = range-unit "=" range-set
//	range-set      = 1#range-spec
//	range-spec     = int-range / suffix-range / other-range
//	int-range      = first-pos "-" [ last-pos ]
//	suffix-range   = "-" suffix-length
//
// The unit is compared case-insensitively and only "bytes" is understood.
// Unsatisfiable specs (first-pos >= size, suffix-length 0) are dropped; a
// last-pos past the end is clamped to size-1 and a suffix longer than the
// representation selects all of it, as §14.1.2 requires. Numbers too large
// for int64 are therefore not errors: a huge last-pos or suffix-length clamps
// and a huge first-pos is unsatisfiable. Ranges are returned in request order,
// neither sorted nor coalesced.
//
// A nil error always comes with at least one range. size <= 0 makes every
// range unsatisfiable.
func ParseRange(s string, size int64) ([]ByteRange, error) {
	unit, set, ok := strings.Cut(s, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil, ErrRangeIgnored
	}
	var (
		out   []ByteRange
		specs int
		total int64
	)
	for spec := range strings.SplitSeq(set, ",") {
		spec = strings.Trim(spec, " \t")
		if spec == "" {
			continue // #rule: empty list elements are allowed (RFC 9110 §5.6.1)
		}
		specs++
		if specs > MaxRanges {
			return nil, ErrRangeIgnored
		}
		first, last, ok := strings.Cut(spec, "-")
		if !ok {
			return nil, ErrRangeIgnored
		}
		var r ByteRange
		switch first {
		case "":
			n, ok := parsePos(last)
			if !ok {
				return nil, ErrRangeIgnored
			}
			if n == 0 || size <= 0 {
				continue // a zero suffix-length is unsatisfiable
			}
			n = min(n, size)
			r = ByteRange{Start: size - n, Length: n}
		default:
			start, ok := parsePos(first)
			if !ok {
				return nil, ErrRangeIgnored
			}
			end := int64(math.MaxInt64)
			if last != "" {
				if end, ok = parsePos(last); !ok {
					return nil, ErrRangeIgnored
				}
				if end < start {
					return nil, ErrRangeIgnored // §14.1.1: last-pos < first-pos is invalid
				}
			}
			if start >= size {
				continue
			}
			end = min(end, size-1)
			r = ByteRange{Start: start, Length: end - start + 1}
		}
		if r.Length > size-total { // total + r.Length > size, without overflow
			// More bytes than the whole body (overlapping or repeated ranges):
			// ignore the header and send the representation once.
			return nil, ErrRangeIgnored
		}
		total += r.Length
		out = append(out, r)
	}
	if specs == 0 {
		return nil, ErrRangeIgnored // range-set is 1#: at least one spec
	}
	if len(out) == 0 {
		return nil, ErrRangeUnsatisfiable
	}
	return out, nil
}

// parsePos parses 1*DIGIT. Values past int64 saturate at MaxInt64 (the callers
// clamp or drop them); anything that is not all digits fails.
func parsePos(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (math.MaxInt64-int64(c-'0'))/10 {
			n = math.MaxInt64
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// IfRange evaluates an If-Range precondition (RFC 9110 §13.1.5) against the
// stored representation's ETag and Last-Modified field values; it reports
// whether the Range may be honoured. An entity-tag matches only by the strong
// comparison (a weak tag on either side never matches); an HTTP-date matches
// only when it exactly equals a valid Last-Modified. An empty ifRange is true
// (no precondition). When it is false the server ignores Range and sends 200.
func IfRange(ifRange, etag, lastModified string) bool {
	v := strings.TrimSpace(ifRange)
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		return strongETagEqual(v, strings.TrimSpace(etag))
	}
	t, ok := ParseDate(v, time.Time{})
	if !ok {
		return false
	}
	lm, ok := ParseDate(lastModified, time.Time{})
	return ok && t.Equal(lm)
}

// strongETagEqual is RFC 9110 §8.8.3.2's strong comparison: both tags are
// strong (no W/ prefix), well formed, and their opaque-tags are identical.
func strongETagEqual(a, b string) bool {
	if strings.HasPrefix(a, "W/") || strings.HasPrefix(b, "W/") {
		return false
	}
	if len(a) < 2 || a[0] != '"' || a[len(a)-1] != '"' {
		return false
	}
	return a == b
}
