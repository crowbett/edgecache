// Package cachekey derives the primary and secondary cache keys of RFC 9111 §4
// from a request: a canonical URL form, sha256 digests as fixed [32]byte values
// (no alloc, no escape), and Vary normalisation.
//
// Primary and Secondary build their input in a fixed-size stack scratch buffer
// and hash it with sha256.Sum256, so the steady-state cost is 0 allocs/op; only
// a URL longer than the scratch buffer spills to the heap.
package cachekey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Key is a sha256 digest. It is an array, never a slice: no alloc, no escape,
// usable as a map key and as the content address of a disk file.
type Key [32]byte

// ErrVaryStar is returned by NormalizeVary when the list contains "*", which
// RFC 9111 §4.1 makes uncacheable.
var ErrVaryStar = errors.New("cachekey: Vary: *")

// String returns the lowercase hex form; for logs and admin output only.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// scratchSize is the stack buffer every hot-path function builds its hash
// input in. A canonical URL plus its query (written twice while sorting) or a
// primary key plus the Vary'd header values must fit for the 0-alloc path.
const scratchSize = 1024

// maxStackPairs is how many query pairs are sorted without touching the heap.
const maxStackPairs = 64

// Canonical returns the canonical URL form used for the primary key: scheme and
// host lowercased, the scheme's default port dropped, percent-encoded unreserved
// characters decoded (and remaining escapes uppercased) in the path and query,
// the query pairs sorted by their bytes (key then value), empty pairs dropped,
// and an empty path rendered as "/". The fragment is never part of the key.
func Canonical(u *url.URL) string {
	var arr [scratchSize]byte
	return string(appendCanonical(arr[:0], u))
}

// Primary returns sha256(method \x00 canonical URL).
func Primary(method string, u *url.URL) Key {
	var arr [scratchSize]byte
	buf := append(arr[:0], method...)
	buf = append(buf, 0)
	buf = appendCanonical(buf, u)
	return Key(sha256.Sum256(buf))
}

// Secondary returns sha256(p ‖ name \x00 value \x00 ...) over the request's
// values of the (already normalised) Vary field names, in list order. Header
// values are joined with ", " and whitespace-trimmed; a missing header hashes as
// an empty value.
func Secondary(p Key, vary []string, h http.Header) Key {
	var arr [scratchSize]byte
	var nameArr [64]byte
	buf := append(arr[:0], p[:]...)
	for _, name := range vary {
		buf = append(buf, name...)
		buf = append(buf, 0)
		for i, v := range headerValues(h, name, nameArr[:0]) {
			if i > 0 {
				buf = append(buf, ',', ' ')
			}
			buf = append(buf, strings.TrimSpace(v)...)
		}
		buf = append(buf, 0)
	}
	return Key(sha256.Sum256(buf))
}

// NormalizeVary lowercases, trims, sorts and dedupes the field names of one or
// more Vary values (each may itself be a comma list). It returns ErrVaryStar when
// any element is "*".
func NormalizeVary(v []string) ([]string, error) {
	var out []string
	for _, raw := range v {
		for part := range strings.SplitSeq(raw, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "*" {
				return nil, ErrVaryStar
			}
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// appendCanonical appends the canonical form of u to buf and returns the
// extended slice. It never allocates unless buf runs out of capacity.
func appendCanonical(buf []byte, u *url.URL) []byte {
	if u.Scheme != "" {
		buf = appendLower(buf, u.Scheme)
		buf = append(buf, "://"...)
	}
	hostStart := len(buf)
	buf = appendLower(buf, u.Host)
	host := buf[hostStart:]
	if i := bytes.LastIndexByte(host, ':'); i >= 0 && bytes.IndexByte(host[i:], ']') < 0 &&
		isDefaultPort(u.Scheme, host[i+1:]) {
		buf = buf[:hostStart+i]
	}
	switch {
	case u.RawPath != "" && rawPathMatches(u.RawPath, u.Path):
		buf = appendNormalized(buf, u.RawPath)
	case u.Path == "":
		buf = append(buf, '/')
	default:
		buf = appendNormalized(buf, u.EscapedPath())
	}
	if u.RawQuery != "" {
		buf = appendSortedQuery(buf, u.RawQuery)
	}
	return buf
}

// appendSortedQuery appends "?" plus the escape-normalised, byte-sorted,
// non-empty pairs of raw. Pairs are first written in place after the path,
// then copied in sorted order to the tail and moved back, so one buffer does.
func appendSortedQuery(buf []byte, raw string) []byte {
	qStart := len(buf)
	buf = append(buf, '?')
	var spanArr [maxStackPairs][2]int32
	spans := spanArr[:0]
	for raw != "" {
		var part string
		if i := strings.IndexByte(raw, '&'); i >= 0 {
			part, raw = raw[:i], raw[i+1:]
		} else {
			part, raw = raw, ""
		}
		if part == "" {
			continue
		}
		s := len(buf)
		buf = appendNormalized(buf, part)
		spans = append(spans, [2]int32{int32(s), int32(len(buf))}) //nolint:gosec // bounded by len(buf) < 2^31
	}
	if len(spans) == 0 {
		return buf[:qStart]
	}
	sortSpans(buf, spans)
	end := len(buf)
	for i, sp := range spans {
		if i > 0 {
			buf = append(buf, '&')
		}
		buf = append(buf, buf[sp[0]:sp[1]]...)
	}
	n := copy(buf[qStart+1:], buf[end:])
	return buf[:qStart+1+n]
}

// sortSpans orders spans by the bytes they address in buf. Insertion sort keeps
// the common short query alloc-free; long ones fall back to slices.SortFunc.
func sortSpans(buf []byte, spans [][2]int32) {
	if len(spans) > 32 {
		slices.SortFunc(spans, func(a, b [2]int32) int {
			return bytes.Compare(buf[a[0]:a[1]], buf[b[0]:b[1]])
		})
		return
	}
	for i := 1; i < len(spans); i++ {
		cur := spans[i]
		j := i
		for j > 0 && bytes.Compare(buf[spans[j-1][0]:spans[j-1][1]], buf[cur[0]:cur[1]]) > 0 {
			spans[j] = spans[j-1]
			j--
		}
		spans[j] = cur
	}
}

// headerValues returns h's values for the lowercase field name without
// allocating: the canonical form is built in scratch and looked up directly.
func headerValues(h http.Header, name string, scratch []byte) []string {
	if len(name) > cap(scratch) {
		return h.Values(name)
	}
	scratch = scratch[:len(name)]
	upper := true
	for i := range len(name) {
		c := name[i]
		if !isTokenByte(c) {
			return h.Values(name)
		}
		switch {
		case upper && 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
		case !upper && 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		scratch[i] = c
		upper = c == '-'
	}
	return h[string(scratch)]
}

func isDefaultPort(scheme string, port []byte) bool {
	switch {
	case strings.EqualFold(scheme, "http"):
		return string(port) == "80"
	case strings.EqualFold(scheme, "https"):
		return string(port) == "443"
	}
	return false
}

func appendLower(buf []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf = append(buf, c)
	}
	return buf
}

// rawPathMatches reports whether raw is a valid encoded path that decodes to
// path, i.e. whether url.URL.EscapedPath would return it. Doing the check here
// avoids the decode allocation inside EscapedPath.
func rawPathMatches(raw, path string) bool {
	j := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '%' {
			if i+2 >= len(raw) || !isHex(raw[i+1]) || !isHex(raw[i+2]) {
				return false
			}
			c = unhex(raw[i+1])<<4 | unhex(raw[i+2])
			i += 2
		} else if !validPathByte(c) {
			return false
		}
		if j >= len(path) || path[j] != c {
			return false
		}
		j++
	}
	return j == len(path)
}

// validPathByte mirrors net/url's validEncoded for encodePath: pchar minus
// pct-encoded, plus "[" and "]".
func validPathByte(c byte) bool {
	if isUnreserved(c) {
		return true
	}
	switch c {
	case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@', '/', '[', ']':
		return true
	}
	return false
}

// appendNormalized appends s with %XX escapes of unreserved characters (ALPHA /
// DIGIT / "-" / "." / "_" / "~") decoded and every other escape's hex uppercased.
func appendNormalized(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' || i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			buf = append(buf, c)
			continue
		}
		v := unhex(s[i+1])<<4 | unhex(s[i+2])
		if isUnreserved(v) {
			buf = append(buf, v)
		} else {
			buf = append(buf, '%', upperHex(s[i+1]), upperHex(s[i+2]))
		}
		i += 2
	}
	return buf
}

func upperHex(c byte) byte {
	if 'a' <= c && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isUnreserved(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// isTokenByte is RFC 9110 tchar: the bytes allowed in a field name.
func isTokenByte(c byte) bool {
	if isUnreserved(c) {
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '^', '`', '|':
		return true
	}
	return false
}
