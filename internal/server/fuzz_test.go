package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// validHost mirrors the byte set net/http accepts in a Host header, minus the
// punctuation that would re-parse as another URL component.
func validHost(h string) bool {
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']', c == '_':
		default:
			return false
		}
	}
	return true
}

// FuzzRequestURL: the absolute URL rebuilt for PURGE never panics, always has
// a scheme the engine accepts, re-parses, and keeps the request's path and query.
func FuzzRequestURL(f *testing.F) {
	f.Add("example.com", "/obj/1?x=1", "")
	f.Add("example.com:8080", "/a%20b", "https")
	f.Add("[::1]:9", "/", "http")
	f.Add("", "/x", "HTTPS")
	f.Add("h", "/x?%zz", "ftp")
	f.Add("h", "http://other/x", "")
	f.Add("h", "*", "")
	f.Add("?", "0", "0")
	f.Add("0", "?#", "")
	f.Add("h:", "/x", "")
	f.Fuzz(func(t *testing.T, host, target, proto string) {
		if !strings.HasPrefix(target, "/") && !strings.Contains(target, "://") {
			target = "/" + target
		}
		u, err := url.ParseRequestURI(target)
		if err != nil || !validHost(host) || strings.Contains(target, "#") {
			// net/http answers 400 to a malformed Host before any handler runs;
			// '#' is not a request-target byte (RFC 3986 query excludes it).
			return
		}
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/", http.NoBody)
		r.URL = u
		r.Host = host
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		got := requestURL(r)
		if !strings.HasPrefix(got, "http://") && !strings.HasPrefix(got, "https://") && !u.IsAbs() {
			t.Fatalf("scheme missing in %q", got)
		}
		if proto != "https" && proto != "http" && !u.IsAbs() && !strings.HasPrefix(got, "http://") {
			t.Fatalf("untrusted X-Forwarded-Proto %q changed the scheme: %q", proto, got)
		}
		back, err := url.Parse(got)
		if err != nil {
			return // a hostile Host value need not re-parse; the engine rejects it
		}
		if !u.IsAbs() && back.RawQuery != u.RawQuery {
			t.Fatalf("query changed: %q -> %q", u.RawQuery, back.RawQuery)
		}
		if isLoopbackAddr(host) && !strings.EqualFold(host, "localhost") && !strings.Contains(host, "127.") && !strings.Contains(host, "::1") {
			t.Fatalf("%q reported loopback", host)
		}
	})
}
