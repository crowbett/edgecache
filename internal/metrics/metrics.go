// Package metrics is a hand-written Prometheus text registry (no client_golang,
// aigw/sysmon style) exposing the edgecache_* families with fixed-enum labels
// only, plus a JSON snapshot for /admin/stats. It satisfies cache.Recorder.
//
// Every label value comes from a closed set (outcome, status_class, tier,
// kind); an unknown value is folded into a catch-all rather than creating a new
// series, so a hostile client can never grow cardinality. The exposition is
// rendered on a pooled buffer under one mutex; a scrape costs a few
// microseconds and produces almost no garbage.
package metrics

import (
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowbett/edgecache/internal/store"
)

// ContentType is the Prometheus text exposition media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Outcome label values of edgecache_requests_total, in exposition order. They
// mirror the cache.Outcome* constants; anything else is folded into "error".
var Outcomes = [...]string{"hit", "miss", "stale", "revalidated", "bypass", "error"}

// StatusClasses are the status_class label values, in exposition order. "error"
// is a transport failure (no status), "other" a status outside 100-599.
var StatusClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx", "error", "other"}

// PurgeKinds are the kind label values of edgecache_purges_total.
var PurgeKinds = [...]string{"url", "prefix", "tag", "other"}

// Histogram bucket upper bounds, in seconds. A cache hit lands in the first
// buckets; a slow origin (50 ms +/- jitter) in the middle; collapse waits and
// large bodies at the top.
var (
	DurationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	OriginBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
)

const (
	outcomeError = 5 // index of "error" in Outcomes
	classError   = 5 // index of "error" in StatusClasses
	classOther   = 6 // index of "other" in StatusClasses
	kindOther    = 3 // index of "other" in PurgeKinds
)

// Registry holds every counter, gauge and histogram. All methods are safe for
// concurrent use.
type Registry struct {
	mu sync.Mutex

	requests  [len(Outcomes)]uint64
	respBytes [len(Outcomes)]uint64
	responses [len(StatusClasses)]uint64
	duration  histogram
	ttfb      [len(Outcomes)]histogram

	originReqs [len(StatusClasses)]uint64
	originDur  histogram

	collapsed        uint64
	purges           [len(PurgeKinds)]uint64
	variantEvictions uint64

	tiers []string // sorted keys of stats
	stats map[string]store.Stats

	// storeFn, when set, is called on every scrape/snapshot to refresh the
	// per-tier gauges (main wires it to the store's Stats).
	storeFn func() map[string]store.Stats

	build     map[string]string
	buildInfo []byte // pre-rendered "edgecache_build_info{...} 1\n" block
	started   time.Time
}

// NewRegistry creates a registry whose edgecache_build_info carries the given
// labels (version, go_version, ...). "go_version" defaults to runtime.Version()
// and "version" to "dev" when absent.
func NewRegistry(build map[string]string) *Registry {
	labels := make(map[string]string, len(build)+2)
	for k, v := range build {
		labels[k] = v
	}
	if _, ok := labels["go_version"]; !ok {
		labels["go_version"] = runtime.Version()
	}
	if _, ok := labels["version"]; !ok {
		labels["version"] = "dev"
	}
	m := &Registry{
		duration:  newHistogram(DurationBuckets),
		originDur: newHistogram(OriginBuckets),
		stats:     map[string]store.Stats{},
		build:     labels,
		buildInfo: renderBuildInfo(labels),
		started:   time.Now(),
	}
	for i := range m.ttfb {
		m.ttfb[i] = newHistogram(DurationBuckets)
	}
	return m
}

// SetStoreStatsFunc registers the callback that refreshes the per-tier gauges
// (edgecache_store_bytes, _entries, _capacity_bytes, edgecache_evictions_total)
// right before every scrape and JSON snapshot. main passes the store's Stats
// method; the janitor may still push StoreGauges between scrapes.
func (m *Registry) SetStoreStatsFunc(fn func() map[string]store.Stats) {
	m.mu.Lock()
	m.storeFn = fn
	m.mu.Unlock()
}

// Request records one client request: outcome label, status, time to first
// byte, total duration and bytes written. A ttfb of zero (nothing was written)
// is not observed.
func (m *Registry) Request(outcome string, status int, ttfb, total time.Duration, size int64) {
	o := outcomeIndex(outcome)
	c := statusClass(status)
	m.mu.Lock()
	m.requests[o]++
	m.responses[c]++
	if size > 0 {
		m.respBytes[o] += uint64(size)
	}
	m.duration.observe(total.Seconds())
	if ttfb > 0 {
		m.ttfb[o].observe(ttfb.Seconds())
	}
	m.mu.Unlock()
}

// Origin records one upstream exchange; err non-nil counts as status_class="error".
func (m *Registry) Origin(status int, d time.Duration, err error) {
	c := statusClass(status)
	if err != nil {
		c = classError
	}
	m.mu.Lock()
	m.originReqs[c]++
	m.originDur.observe(d.Seconds())
	m.mu.Unlock()
}

// Collapsed adds waiters to edgecache_collapsed_total.
func (m *Registry) Collapsed(waiters int) {
	if waiters <= 0 {
		return
	}
	m.mu.Lock()
	m.collapsed += uint64(waiters)
	m.mu.Unlock()
}

// StoreGauges refreshes the per-tier bytes/entries/capacity gauges and the
// evictions counter from a Stats map keyed by tier ("mem", "disk").
func (m *Registry) StoreGauges(stats map[string]store.Stats) {
	m.mu.Lock()
	m.setStats(stats)
	m.mu.Unlock()
}

// setStats replaces the tier snapshot; the caller holds mu.
func (m *Registry) setStats(stats map[string]store.Stats) {
	m.tiers = m.tiers[:0]
	for k := range m.stats {
		if _, ok := stats[k]; !ok {
			delete(m.stats, k)
		}
	}
	for k, v := range stats {
		m.stats[k] = v
		m.tiers = append(m.tiers, k)
	}
	slices.Sort(m.tiers)
}

// Purged adds n to edgecache_purges_total{kind}; kind is url|prefix|tag.
func (m *Registry) Purged(kind string, n int) {
	if n < 0 {
		return
	}
	k := kindOther
	for i, v := range PurgeKinds {
		if v == kind {
			k = i
			break
		}
	}
	m.mu.Lock()
	m.purges[k] += uint64(n)
	m.mu.Unlock()
}

// VariantEvicted increments edgecache_variant_evictions_total.
func (m *Registry) VariantEvicted() {
	m.mu.Lock()
	m.variantEvictions++
	m.mu.Unlock()
}

// Handler serves the text exposition (text/plain; version=0.0.4).
func (m *Registry) Handler() http.Handler { return m }

type pooledBuf struct{ b []byte }

var bufPool = sync.Pool{New: func() any { return &pooledBuf{b: make([]byte, 0, 16<<10)} }}

// ServeHTTP renders the exposition on a pooled buffer; Handler is the same thing.
func (m *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	pb, _ := bufPool.Get().(*pooledBuf)
	defer bufPool.Put(pb)

	pb.b = m.render(pb.b[:0])

	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(pb.b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pb.b)
}

// refresh pulls the store gauges through the callback (outside the lock: the
// store takes its own mutexes) and installs them.
func (m *Registry) refresh() {
	m.mu.Lock()
	fn := m.storeFn
	m.mu.Unlock()
	if fn == nil {
		return
	}
	stats := fn()
	m.mu.Lock()
	m.setStats(stats)
	m.mu.Unlock()
}

// render appends the whole exposition to buf in a deterministic order.
func (m *Registry) render(buf []byte) []byte {
	m.refresh()
	e := exposer{b: buf}
	m.mu.Lock()
	defer m.mu.Unlock()

	e.header("edgecache_requests_total", "Client requests handled, by cache outcome.", "counter")
	for i, o := range Outcomes {
		e.labelled1("edgecache_requests_total", "outcome", o)
		e.uint(m.requests[i])
	}
	e.header("edgecache_responses_total", "Client responses, by status class.", "counter")
	for i, c := range StatusClasses {
		e.labelled1("edgecache_responses_total", "status_class", c)
		e.uint(m.responses[i])
	}
	e.header("edgecache_response_bytes_total", "Body bytes written to clients, by cache outcome.", "counter")
	for i, o := range Outcomes {
		e.labelled1("edgecache_response_bytes_total", "outcome", o)
		e.uint(m.respBytes[i])
	}
	e.histogram("edgecache_request_duration_seconds", "End-to-end client request duration in seconds.", &m.duration)
	e.header("edgecache_ttfb_seconds", "Time to first byte written to the client in seconds, by cache outcome.", "histogram")
	for i, o := range Outcomes {
		e.histogramSeries("edgecache_ttfb_seconds", `outcome="`+o+`",`, `outcome="`+o+`"`, &m.ttfb[i])
	}

	e.header("edgecache_origin_requests_total", "Upstream exchanges, by status class (error = transport failure).", "counter")
	for i, c := range StatusClasses {
		e.labelled1("edgecache_origin_requests_total", "status_class", c)
		e.uint(m.originReqs[i])
	}
	e.histogram("edgecache_origin_duration_seconds", "Upstream exchange duration in seconds (headers received).", &m.originDur)

	e.header("edgecache_collapsed_total", "Requests that waited on another request's origin fetch (collapse waiters).", "counter")
	e.str("edgecache_collapsed_total ")
	e.uint(m.collapsed)

	e.header("edgecache_store_bytes", "Bytes resident in the tier.", "gauge")
	for _, t := range m.tiers {
		e.labelled1("edgecache_store_bytes", "tier", t)
		e.int(m.stats[t].Bytes)
	}
	e.header("edgecache_store_entries", "Entries resident in the tier.", "gauge")
	for _, t := range m.tiers {
		e.labelled1("edgecache_store_entries", "tier", t)
		e.int(m.stats[t].Entries)
	}
	e.header("edgecache_store_capacity_bytes", "Byte budget of the tier.", "gauge")
	for _, t := range m.tiers {
		e.labelled1("edgecache_store_capacity_bytes", "tier", t)
		e.int(m.stats[t].Capacity)
	}
	e.header("edgecache_evictions_total", "Entries evicted by the tier's byte budget.", "counter")
	for _, t := range m.tiers {
		e.labelled1("edgecache_evictions_total", "tier", t)
		e.int(m.stats[t].Evictions)
	}

	e.header("edgecache_purges_total", "Entries removed by purge, by selector kind.", "counter")
	for i, k := range PurgeKinds {
		e.labelled1("edgecache_purges_total", "kind", k)
		e.uint(m.purges[i])
	}
	e.header("edgecache_variant_evictions_total", "Vary variants dropped by the per-key variant cap.", "counter")
	e.str("edgecache_variant_evictions_total ")
	e.uint(m.variantEvictions)

	e.b = append(e.b, m.buildInfo...)
	return e.b
}

// Snapshot is the JSON shape of SnapshotJSON.
type Snapshot struct {
	Requests         map[string]uint64           `json:"requests"`
	Responses        map[string]uint64           `json:"responses"`
	ResponseBytes    map[string]uint64           `json:"response_bytes"`
	RequestDuration  HistogramSummary            `json:"request_duration_seconds"`
	TTFB             map[string]HistogramSummary `json:"ttfb_seconds"`
	OriginRequests   map[string]uint64           `json:"origin_requests"`
	OriginDuration   HistogramSummary            `json:"origin_duration_seconds"`
	Collapsed        uint64                      `json:"collapsed"`
	Store            map[string]store.Stats      `json:"store"`
	Purges           map[string]uint64           `json:"purges"`
	VariantEvictions uint64                      `json:"variant_evictions"`
	HitRatio         float64                     `json:"hit_ratio"`
	Build            map[string]string           `json:"build"`
	UptimeSeconds    float64                     `json:"uptime_seconds"`
}

// HistogramSummary is a histogram reduced to count, sum, mean and bucket-estimated
// quantiles (the same interpolation Prometheus' histogram_quantile uses).
type HistogramSummary struct {
	Count uint64  `json:"count"`
	Sum   float64 `json:"sum"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
}

// SnapshotJSON renders the current values as JSON for /admin/stats.
func (m *Registry) SnapshotJSON() []byte {
	b, err := json.Marshal(m.Snapshot())
	if err != nil { // cannot happen: every field is a plain map/number
		return []byte(`{"error":"snapshot"}`)
	}
	return b
}

// Snapshot returns the current values as a struct (SnapshotJSON marshals it).
func (m *Registry) Snapshot() Snapshot {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Snapshot{
		Requests:         make(map[string]uint64, len(Outcomes)),
		Responses:        make(map[string]uint64, len(StatusClasses)),
		ResponseBytes:    make(map[string]uint64, len(Outcomes)),
		RequestDuration:  m.duration.summary(),
		TTFB:             make(map[string]HistogramSummary, len(Outcomes)),
		OriginRequests:   make(map[string]uint64, len(StatusClasses)),
		OriginDuration:   m.originDur.summary(),
		Collapsed:        m.collapsed,
		Store:            make(map[string]store.Stats, len(m.stats)),
		Purges:           make(map[string]uint64, len(PurgeKinds)),
		VariantEvictions: m.variantEvictions,
		Build:            m.build,
		UptimeSeconds:    time.Since(m.started).Seconds(),
	}
	var total, served uint64
	for i, o := range Outcomes {
		s.Requests[o] = m.requests[i]
		s.ResponseBytes[o] = m.respBytes[i]
		s.TTFB[o] = m.ttfb[i].summary()
		total += m.requests[i]
		if o == "hit" || o == "stale" || o == "revalidated" {
			served += m.requests[i]
		}
	}
	if total > 0 {
		s.HitRatio = float64(served) / float64(total)
	}
	for i, c := range StatusClasses {
		s.Responses[c] = m.responses[i]
		s.OriginRequests[c] = m.originReqs[i]
	}
	for k, v := range m.stats {
		s.Store[k] = v
	}
	for i, k := range PurgeKinds {
		s.Purges[k] = m.purges[i]
	}
	return s
}

func outcomeIndex(outcome string) int {
	for i, o := range Outcomes {
		if o == outcome {
			return i
		}
	}
	return outcomeError
}

func statusClass(status int) int {
	if status < 100 || status > 599 {
		return classOther
	}
	return status/100 - 1
}

// histogram is a fixed-bucket histogram; counts[i] is the number of
// observations in (bounds[i-1], bounds[i]] and counts[len(bounds)] the +Inf
// overflow. Cumulation happens at render time.
type histogram struct {
	bounds []float64
	le     []string
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) histogram {
	le := make([]string, len(bounds))
	for i, b := range bounds {
		le[i] = strconv.FormatFloat(b, 'g', -1, 64)
	}
	return histogram{bounds: bounds, le: le, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v; le is inclusive
	h.counts[i]++
	h.sum += v
	h.count++
}

func (h *histogram) summary() HistogramSummary {
	s := HistogramSummary{Count: h.count, Sum: h.sum}
	if h.count == 0 {
		return s
	}
	s.Mean = h.sum / float64(h.count)
	s.P50 = h.quantile(0.5)
	s.P90 = h.quantile(0.9)
	s.P99 = h.quantile(0.99)
	return s
}

// quantile estimates q by linear interpolation inside the bucket that crosses
// rank q*count; the +Inf bucket reports the last finite bound.
func (h *histogram) quantile(q float64) float64 {
	rank := q * float64(h.count)
	var cum uint64
	for i, c := range h.counts {
		prev := cum
		cum += c
		if float64(cum) < rank {
			continue
		}
		if i == len(h.bounds) {
			return h.bounds[len(h.bounds)-1]
		}
		lo := 0.0
		if i > 0 {
			lo = h.bounds[i-1]
		}
		hi := h.bounds[i]
		if c == 0 {
			return hi
		}
		return lo + (hi-lo)*(rank-float64(prev))/float64(c)
	}
	return h.bounds[len(h.bounds)-1]
}

// exposer is a tiny append-only writer for the text format.
type exposer struct{ b []byte }

func (e *exposer) str(s string) { e.b = append(e.b, s...) }
func (e *exposer) byte(c byte)  { e.b = append(e.b, c) }

func (e *exposer) header(name, help, kind string) {
	e.str("# HELP ")
	e.str(name)
	e.byte(' ')
	e.str(help)
	e.str("\n# TYPE ")
	e.str(name)
	e.byte(' ')
	e.str(kind)
	e.byte('\n')
}

func (e *exposer) float(v float64) {
	e.b = strconv.AppendFloat(e.b, v, 'g', -1, 64)
	e.byte('\n')
}

func (e *exposer) uint(v uint64) {
	e.b = strconv.AppendUint(e.b, v, 10)
	e.byte('\n')
}

func (e *exposer) int(v int64) {
	e.b = strconv.AppendInt(e.b, v, 10)
	e.byte('\n')
}

// labelled1 writes `name{k="v"} ` (value escaped) ready for the sample value.
func (e *exposer) labelled1(name, k, v string) {
	e.str(name)
	e.byte('{')
	e.str(k)
	e.str(`="`)
	e.b = appendEscaped(e.b, v)
	e.str(`"} `)
}

// histogram writes a whole single-series histogram family.
func (e *exposer) histogram(name, help string, h *histogram) {
	e.header(name, help, "histogram")
	e.histogramSeries(name, "", "", h)
}

// histogramSeries writes the _bucket/_sum/_count lines of one series. bucketPfx
// is the label prefix inside the bucket braces (e.g. `outcome="hit",`) and
// plain the label set for _sum/_count (`outcome="hit"`), both empty for an
// unlabelled histogram.
func (e *exposer) histogramSeries(name, bucketPfx, plain string, h *histogram) {
	var cum uint64
	for i, le := range h.le {
		cum += h.counts[i]
		e.str(name)
		e.str("_bucket{")
		e.str(bucketPfx)
		e.str(`le="`)
		e.str(le)
		e.str(`"} `)
		e.uint(cum)
	}
	e.str(name)
	e.str("_bucket{")
	e.str(bucketPfx)
	e.str(`le="+Inf"} `)
	e.uint(h.count)
	e.str(name)
	e.str("_sum")
	e.labels(plain)
	e.float(h.sum)
	e.str(name)
	e.str("_count")
	e.labels(plain)
	e.uint(h.count)
}

func (e *exposer) labels(plain string) {
	if plain == "" {
		e.byte(' ')
		return
	}
	e.byte('{')
	e.str(plain)
	e.str("} ")
}

// appendEscaped appends v escaped per the text format (backslash, double
// quote and newline).
func appendEscaped(b []byte, v string) []byte {
	if !strings.ContainsAny(v, "\\\"\n") { // common case: nothing to escape
		return append(b, v...)
	}
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			b = append(b, '\\', '\\')
		case '"':
			b = append(b, '\\', '"')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, c)
		}
	}
	return b
}

// renderBuildInfo pre-renders the constant edgecache_build_info block with
// labels in sorted key order.
func renderBuildInfo(labels map[string]string) []byte {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b bytes.Buffer
	b.WriteString("# HELP edgecache_build_info Build metadata; always 1.\n# TYPE edgecache_build_info gauge\nedgecache_build_info{")
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.Write(appendEscaped(nil, labels[k]))
		b.WriteByte('"')
	}
	b.WriteString("} 1\n")
	return b.Bytes()
}
