package cache

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/trustproxy"
)

func TestForwardedProtoNeedsTrustedPeer(t *testing.T) {
	t.Parallel()
	trusted := trustproxy.New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	tests := []struct {
		name    string
		list    trustproxy.List
		remote  string
		wantKey string
	}{
		{"no list", trustproxy.List{}, "10.0.0.1:1234", "http://example.com/obj/1"},
		{"untrusted peer", trusted, "192.0.2.1:1234", "http://example.com/obj/1"},
		{"trusted peer", trusted, "10.0.0.1:1234", "https://example.com/obj/1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(c *Config) { c.TrustedProxies = tc.list })
			r := httptest.NewRequest(http.MethodGet, "/obj/1", http.NoBody)
			r.URL.Scheme, r.URL.Host = "", ""
			r.Host = "example.com"
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			h.h.ServeHTTP(w, r)
			expect(t, w, 200, "MISS", "hello")
			want := cachekey.Primary(http.MethodGet, mustURL(t, tc.wantKey)).String()
			if got := w.Header().Get(HeaderXCacheKey); got != want {
				t.Errorf("X-Cache-Key = %s, want the key of %s", got, tc.wantKey)
			}
		})
	}
}
