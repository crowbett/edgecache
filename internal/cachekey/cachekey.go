// Package cachekey derives the primary and secondary cache keys of RFC 9111 §4
// from a request: a canonical URL form, sha256 digests as fixed [32]byte values
// (no alloc, no escape), and Vary normalisation.
//
// Written by the integrator at step 1 to freeze the contract; workstream A owns
// this package and may replace the bodies (0-alloc Secondary, pooled scratch),
// keeping every exported signature.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Key is a sha256 digest. It is an array, never a slice: no alloc, no escape,
// usable as a map key and as the content address of a disk file.
type Key [32]byte

// ErrVaryStar is returned by NormalizeVary when the list contains "*", which
// RFC 9111 §4.1 makes uncacheable.
var ErrVaryStar = errors.New("cachekey: Vary: *")

// String returns the lowercase hex form; for logs and admin output only.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// Canonical returns the canonical URL form used for the primary key: scheme and
// host lowercased, the scheme's default port dropped, percent-encoded unreserved
// characters decoded (and remaining escapes uppercased) in the path, the query
// sorted by key then value, and an empty path rendered as "/". The fragment is
// never part of the key.
func Canonical(u *url.URL) string {
	var b strings.Builder
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") && host[i+1:] == defaultPort(scheme) {
		host = host[:i]
	}
	if scheme != "" {
		b.WriteString(scheme)
		b.WriteString("://")
	}
	b.WriteString(host)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	b.WriteString(normalizeEscapes(path))
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(sortedQuery(u.RawQuery))
	}
	return b.String()
}

// Primary returns sha256(method \x00 canonical URL).
func Primary(method string, u *url.URL) Key {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(Canonical(u)))
	var k Key
	h.Sum(k[:0])
	return k
}

// scratch pools the buffer Secondary hashes through so the hot path does not
// allocate per request.
var scratch = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// Secondary returns sha256(p ‖ name \x00 value \x00 ...) over the request's
// values of the (already normalised) Vary field names, in list order. Header
// values are joined with ", " and whitespace-trimmed; a missing header hashes as
// an empty value.
func Secondary(p Key, vary []string, h http.Header) Key {
	bp, _ := scratch.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, p[:]...)
	for _, name := range vary {
		buf = append(buf, name...)
		buf = append(buf, 0)
		vals := h.Values(name)
		for i, v := range vals {
			if i > 0 {
				buf = append(buf, ',', ' ')
			}
			buf = append(buf, strings.TrimSpace(v)...)
		}
		buf = append(buf, 0)
	}
	k := Key(sha256.Sum256(buf))
	*bp = buf
	scratch.Put(bp)
	return k
}

// NormalizeVary lowercases, trims, sorts and dedupes the field names of one or
// more Vary values (each may itself be a comma list). It returns ErrVaryStar when
// any element is "*".
func NormalizeVary(v []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range v {
		for part := range strings.SplitSeq(raw, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "*" {
				return nil, ErrVaryStar
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// normalizeEscapes decodes %XX escapes of unreserved characters (ALPHA / DIGIT /
// "-" / "." / "_" / "~") and uppercases the hex digits of every other escape.
func normalizeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' || i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			b.WriteByte(c)
			continue
		}
		v := unhex(s[i+1])<<4 | unhex(s[i+2])
		if isUnreserved(v) {
			b.WriteByte(v)
		} else {
			b.WriteByte('%')
			b.WriteByte(strings.ToUpper(s[i+1 : i+2])[0])
			b.WriteByte(strings.ToUpper(s[i+2 : i+3])[0])
		}
		i += 2
	}
	return b.String()
}

// sortedQuery orders the raw query's key=value pairs by key then value, keeping
// each pair's bytes as sent (after escape normalisation) so equivalent encodings
// collapse and distinct ones do not.
func sortedQuery(raw string) string {
	parts := strings.Split(raw, "&")
	pairs := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			pairs = append(pairs, normalizeEscapes(p))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
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
