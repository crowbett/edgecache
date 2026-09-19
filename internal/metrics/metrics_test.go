package metrics

import (
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/metrics.golden")

// fixture drives a fixed, order-independent sequence of updates.
func fixture(t *testing.T) *Registry {
	t.Helper()
	m := NewRegistry(map[string]string{"version": "v0.0.0-test", "go_version": "go1.99"})
	m.Request("hit", 200, 80*time.Microsecond, 300*time.Microsecond, 16384)
	m.Request("hit", 200, 120*time.Microsecond, 450*time.Microsecond, 16384)
	m.Request("miss", 200, 52*time.Millisecond, 55*time.Millisecond, 16384)
	m.Request("stale", 200, 90*time.Microsecond, 400*time.Microsecond, 16384)
	m.Request("revalidated", 304, 48*time.Millisecond, 49*time.Millisecond, 0)
	m.Request("bypass", 404, 51*time.Millisecond, 52*time.Millisecond, 19)
	m.Request("error", 502, 0, 60*time.Second, 0)
	m.Request(`bogus"outcome`, 999, 0, time.Millisecond, 1) // folded into error / other
	m.Origin(200, 51*time.Millisecond, nil)
	m.Origin(304, 47*time.Millisecond, nil)
	m.Origin(503, 30*time.Millisecond, nil)
	m.Origin(0, 10*time.Second, errors.New("dial tcp: refused"))
	m.Collapsed(3)
	m.Collapsed(0)
	m.Collapsed(-1)
	m.StoreGauges(map[string]store.Stats{
		"disk": {Bytes: 1 << 30, Entries: 65536, Evictions: 12, Capacity: 5 << 30},
		"mem":  {Bytes: 1 << 20, Entries: 64, Evictions: 3, Capacity: 512 << 20},
	})
	m.Purged("url", 2)
	m.Purged("tag", 40)
	m.Purged("prefix", 0)
	m.Purged("nope", 1)
	m.Purged("url", -5)
	m.VariantEvicted()
	m.VariantEvicted()
	return m
}

func scrape(t *testing.T, m *Registry) (string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String(), rec.Header()
}

func TestGolden(t *testing.T) {
	t.Parallel()
	got, hdr := scrape(t, fixture(t))
	if ct := hdr.Get("Content-Type"); ct != ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ContentType)
	}
	if cl := hdr.Get("Content-Length"); cl != strconv.Itoa(len(got)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(got))
	}

	path := filepath.Join("testdata", "metrics.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantB, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create)", err)
	}
	want := strings.ReplaceAll(string(wantB), "\r\n", "\n")
	if got != want {
		t.Errorf("exposition differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

var (
	commentRe = regexp.MustCompile(`^# (HELP|TYPE) [a-zA-Z_:][a-zA-Z0-9_:]* \S.*$`)
	sampleRe  = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\["\\n])*"(,[a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\["\\n])*")*)?\})? (-?[0-9]+(\.[0-9]+)?(e[+-]?[0-9]+)?|\+Inf|-Inf|NaN)$`)
)

// Families every scrape must expose, with a TYPE line and at least one sample.
var families = []string{
	"edgecache_requests_total", "edgecache_responses_total", "edgecache_response_bytes_total",
	"edgecache_request_duration_seconds", "edgecache_ttfb_seconds",
	"edgecache_origin_requests_total", "edgecache_origin_duration_seconds",
	"edgecache_collapsed_total", "edgecache_store_bytes", "edgecache_store_entries",
	"edgecache_store_capacity_bytes", "edgecache_evictions_total", "edgecache_purges_total",
	"edgecache_variant_evictions_total", "edgecache_build_info",
}

// TestParse checks every line is well-formed, each sample follows its own
// TYPE line, and the histograms are internally consistent.
func TestParse(t *testing.T) {
	t.Parallel()
	got, _ := scrape(t, fixture(t))
	if !strings.HasSuffix(got, "\n") {
		t.Fatal("exposition must end with a newline")
	}
	types := map[string]string{}
	seen := map[string]bool{}
	var lastCum uint64
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		switch {
		case commentRe.MatchString(line):
			f := strings.Fields(line)
			if f[1] == "TYPE" {
				if _, dup := types[f[2]]; dup {
					t.Errorf("line %d: family %s has two TYPE lines", i+1, f[2])
				}
				types[f[2]] = f[3]
			}
		case sampleRe.MatchString(line):
			sub := sampleRe.FindStringSubmatch(line)
			name := sub[1]
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			kind, ok := types[base]
			if !ok {
				t.Errorf("line %d: sample %q before its TYPE", i+1, name)
			}
			seen[base] = true
			if kind == "histogram" && strings.HasSuffix(name, "_bucket") {
				v, _ := strconv.ParseUint(sub[7], 10, 64)
				if strings.Contains(line, `le="+Inf"`) {
					lastCum = 0 // next series starts over
				} else {
					if v < lastCum {
						t.Errorf("line %d: bucket not monotone: %s", i+1, line)
					}
					lastCum = v
				}
			}
		default:
			t.Errorf("line %d: malformed: %q", i+1, line)
		}
	}
	for _, fam := range families {
		if _, ok := types[fam]; !ok {
			t.Errorf("family %s missing TYPE", fam)
		}
		if !seen[fam] {
			t.Errorf("family %s has no samples", fam)
		}
	}
	for _, h := range []string{"edgecache_request_duration_seconds", "edgecache_origin_duration_seconds"} {
		if inf, cnt := lineValue(t, got, h+`_bucket{le="+Inf"}`), lineValue(t, got, h+"_count"); inf != cnt {
			t.Errorf("%s: +Inf bucket %s != count %s", h, inf, cnt)
		}
	}
	checks := map[string]string{
		`edgecache_requests_total{outcome="hit"}`:                         "2",
		`edgecache_requests_total{outcome="error"}`:                       "2", // one real + one folded
		`edgecache_responses_total{status_class="2xx"}`:                   "4",
		`edgecache_responses_total{status_class="other"}`:                 "1",
		`edgecache_response_bytes_total{outcome="hit"}`:                   "32768",
		"edgecache_request_duration_seconds_count":                        "8",
		`edgecache_ttfb_seconds_count{outcome="hit"}`:                     "2",
		`edgecache_ttfb_seconds_count{outcome="error"}`:                   "0", // zero ttfb is not observed
		`edgecache_ttfb_seconds_bucket{outcome="hit",le="+Inf"}`:          "2",
		`edgecache_origin_requests_total{status_class="error"}`:           "1",
		`edgecache_origin_requests_total{status_class="5xx"}`:             "1",
		"edgecache_origin_duration_seconds_count":                         "4",
		"edgecache_collapsed_total":                                       "3",
		`edgecache_store_bytes{tier="mem"}`:                               "1048576",
		`edgecache_evictions_total{tier="disk"}`:                          "12",
		`edgecache_store_capacity_bytes{tier="disk"}`:                     "5368709120",
		`edgecache_purges_total{kind="tag"}`:                              "40",
		`edgecache_purges_total{kind="other"}`:                            "1",
		`edgecache_purges_total{kind="url"}`:                              "2",
		"edgecache_variant_evictions_total":                               "2",
		`edgecache_build_info{go_version="go1.99",version="v0.0.0-test"}`: "1",
	}
	for prefix, want := range checks {
		if v := lineValue(t, got, prefix); v != want {
			t.Errorf("%s = %s, want %s", prefix, v, want)
		}
	}
	if strings.Contains(got, "bogus") {
		t.Error("an unknown outcome leaked into the exposition")
	}
}

func lineValue(t *testing.T, exposition, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			return strings.TrimPrefix(line, prefix+" ")
		}
	}
	t.Fatalf("no line starting with %q", prefix)
	return ""
}

func TestStoreStatsFunc(t *testing.T) {
	t.Parallel()
	m := NewRegistry(nil)
	calls := 0
	m.SetStoreStatsFunc(func() map[string]store.Stats {
		calls++
		return map[string]store.Stats{"mem": {Bytes: int64(calls) * 100, Entries: 1, Capacity: 1000}}
	})
	got, _ := scrape(t, m)
	if v := lineValue(t, got, `edgecache_store_bytes{tier="mem"}`); v != "100" {
		t.Errorf("first scrape bytes = %s, want 100", v)
	}
	got, _ = scrape(t, m)
	if v := lineValue(t, got, `edgecache_store_bytes{tier="mem"}`); v != "200" {
		t.Errorf("second scrape bytes = %s, want 200 (callback not refreshed)", v)
	}
	var s Snapshot
	if err := json.Unmarshal(m.SnapshotJSON(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Store["mem"].Bytes != 300 {
		t.Errorf("snapshot bytes = %d, want 300", s.Store["mem"].Bytes)
	}
	// A tier that disappears from the map disappears from the exposition.
	m.StoreGauges(map[string]store.Stats{"disk": {Capacity: 1}})
	m.SetStoreStatsFunc(nil)
	got, _ = scrape(t, m)
	if strings.Contains(got, `tier="mem"`) {
		t.Error("stale tier still exposed after StoreGauges without it")
	}
}

func TestSnapshotJSON(t *testing.T) {
	t.Parallel()
	raw := fixture(t).SnapshotJSON()
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("snapshot is not JSON: %v\n%s", err, raw)
	}
	if s.Requests["hit"] != 2 || s.Requests["error"] != 2 {
		t.Errorf("requests = %v", s.Requests)
	}
	if s.RequestDuration.Count != 8 || s.RequestDuration.Mean <= 0 {
		t.Errorf("duration = %+v", s.RequestDuration)
	}
	if s.RequestDuration.P50 <= 0 || s.RequestDuration.P99 < s.RequestDuration.P50 {
		t.Errorf("quantiles not ordered: %+v", s.RequestDuration)
	}
	if s.TTFB["hit"].Count != 2 || s.TTFB["error"].Count != 0 {
		t.Errorf("ttfb = %v", s.TTFB)
	}
	if s.OriginRequests["error"] != 1 || s.OriginDuration.Count != 4 {
		t.Errorf("origin = %v / %+v", s.OriginRequests, s.OriginDuration)
	}
	if s.Collapsed != 3 || s.VariantEvictions != 2 || s.Purges["tag"] != 40 {
		t.Errorf("counters: collapsed=%d variant=%d purges=%v", s.Collapsed, s.VariantEvictions, s.Purges)
	}
	if s.Store["mem"].Entries != 64 || s.Store["disk"].Capacity != 5<<30 {
		t.Errorf("store = %v", s.Store)
	}
	// 3 served-from-cache (2 hit + 1 stale + 1 revalidated = 4) of 8.
	if want := 0.5; s.HitRatio != want {
		t.Errorf("hit_ratio = %g, want %g", s.HitRatio, want)
	}
	if s.Build["version"] != "v0.0.0-test" || s.UptimeSeconds < 0 {
		t.Errorf("build=%v uptime=%g", s.Build, s.UptimeSeconds)
	}
	// Every documented key is present in the raw document.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"requests", "responses", "response_bytes", "request_duration_seconds", "ttfb_seconds",
		"origin_requests", "origin_duration_seconds", "collapsed", "store", "purges", "variant_evictions",
		"hit_ratio", "build", "uptime_seconds",
	} {
		if _, ok := keys[k]; !ok {
			t.Errorf("snapshot missing key %q", k)
		}
	}
}

func TestEmptyRegistryParses(t *testing.T) {
	t.Parallel()
	got, _ := scrape(t, NewRegistry(nil))
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !commentRe.MatchString(line) && !sampleRe.MatchString(line) {
			t.Errorf("malformed: %q", line)
		}
	}
	if !strings.Contains(got, `edgecache_requests_total{outcome="miss"} 0`) {
		t.Error("enum series are not pre-initialised to 0")
	}
	if !strings.Contains(got, `go_version="`+runtime.Version()+`"`) {
		t.Error("go_version default missing")
	}
	if strings.Contains(got, "edgecache_store_bytes{") {
		t.Error("store gauges exposed without any tier")
	}
}

func TestHistogramQuantile(t *testing.T) {
	t.Parallel()
	h := newHistogram([]float64{1, 2, 4})
	for _, v := range []float64{0.5, 1.5, 1.5, 3, 3, 3, 10} {
		h.observe(v)
	}
	// 7 observations: bucket counts [1, 2, 3, 1(+Inf)].
	if got := h.quantile(0.5); got < 2 || got > 4 {
		t.Errorf("p50 = %g, want inside (2,4]", got)
	}
	if got := h.quantile(0.99); got != 4 {
		t.Errorf("p99 = %g, want 4 (last finite bound for +Inf)", got)
	}
	if got := h.quantile(0.1); got < 0 || got > 1 {
		t.Errorf("p10 = %g, want inside [0,1]", got)
	}
}

func BenchmarkHandler(b *testing.B) {
	m := NewRegistry(map[string]string{"version": "bench"})
	for i := range 1000 {
		m.Request(Outcomes[i%len(Outcomes)], 200, time.Duration(i)*time.Microsecond, time.Duration(2*i)*time.Microsecond, 16384)
	}
	m.StoreGauges(map[string]store.Stats{"mem": {Bytes: 1, Entries: 1, Capacity: 1}, "disk": {Bytes: 1, Entries: 1, Capacity: 1}})
	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	h := m.Handler()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

// BenchmarkRequest is the per-request recording cost on the hit path.
func BenchmarkRequest(b *testing.B) {
	m := NewRegistry(nil)
	b.ReportAllocs()
	for b.Loop() {
		m.Request("hit", 200, 80*time.Microsecond, 300*time.Microsecond, 16384)
	}
}

func BenchmarkSnapshotJSON(b *testing.B) {
	m := fixture(&testing.T{})
	b.ReportAllocs()
	for b.Loop() {
		_ = m.SnapshotJSON()
	}
}
