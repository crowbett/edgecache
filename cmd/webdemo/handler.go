// Command webdemo is edgecache's in-browser demo bridge, compiled to js/wasm
// by crowbett.github.io: it plays a list of requests through the real cache
// engine (internal/cache over a memstore, wired with the config defaults as
// cmd/edgecache does) against a scripted origin on a simulated clock, and
// reports what the engine answered. Contract:
// https://github.com/crowbett/crowbett.github.io/blob/main/docs/demo-contract.md
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	"github.com/crowbett/edgecache/internal/config"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

const (
	maxRequests = 40
	maxSeconds  = 7 * 24 * 3600
	maxHeaders  = 8
	maxValue    = 512
	demoURL     = "http://demo.edgecache.test/page"
)

// requestHeaders are the client headers a visitor may set. Range is left out
// on purpose: a multi-range answer uses a random multipart boundary.
var requestHeaders = []string{"Accept-Encoding", "Accept-Language", "Cache-Control", "If-Modified-Since", "If-None-Match", "Pragma"}

// epoch is where the simulated clock starts; any fixed instant works.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Origin is what the scripted origin answers. Header values are sent as
// given; ExpiresIn is seconds after each response's Date, LastModifiedAgo
// seconds before the start, and either is omitted when nil.
type Origin struct {
	CacheControl    string `json:"cacheControl"`
	ETag            string `json:"etag"`
	Vary            string `json:"vary"`
	Age             string `json:"age"`
	ExpiresIn       *int64 `json:"expiresIn"`
	LastModifiedAgo *int64 `json:"lastModifiedAgo"`
}

// Request is one client GET at At seconds after the start, with extra
// request headers (Cache-Control, Accept-Language, ...).
type Request struct {
	At      int64             `json:"at"`
	Headers map[string]string `json:"headers"`
}

// TimelineParams is one run: a fresh engine, the origin, the requests.
type TimelineParams struct {
	Origin   Origin    `json:"origin"`
	Requests []Request `json:"requests"`
}

// Step is the engine's answer to one request and the origin traffic it
// caused, including any background revalidation it started.
type Step struct {
	At       int64    `json:"at"`
	Status   int      `json:"status"`
	XCache   string   `json:"xCache"`
	Reason   string   `json:"reason"`
	Age      string   `json:"age"`
	Body     string   `json:"body"`
	Upstream []string `json:"upstream"`
}

// TimelineResult lists one Step per request, in time order.
type TimelineResult struct {
	Steps []Step `json:"steps"`
}

// Handle answers one bridge call. Errors come back as {"error": "..."}.
func Handle(method string, params []byte) (res any) {
	defer func() {
		if v := recover(); v != nil {
			res = errorResult(fmt.Errorf("internal: %v", v))
		}
	}()
	switch method {
	case "timeline":
		var p TimelineParams
		if err := json.Unmarshal(params, &p); err != nil {
			return errorResult(fmt.Errorf("params: %w", err))
		}
		r, err := Timeline(p)
		if err != nil {
			return errorResult(err)
		}
		return r
	default:
		return errorResult(fmt.Errorf("unknown method %q", method))
	}
}

func errorResult(err error) map[string]string {
	return map[string]string{"error": err.Error()}
}

// Timeline plays p's requests in time order through a fresh engine.
func Timeline(p TimelineParams) (TimelineResult, error) {
	if len(p.Requests) == 0 || len(p.Requests) > maxRequests {
		return TimelineResult{}, fmt.Errorf("want 1..%d requests, got %d", maxRequests, len(p.Requests))
	}
	if err := p.Origin.validate(); err != nil {
		return TimelineResult{}, err
	}
	for _, r := range p.Requests {
		if r.At < 0 || r.At > maxSeconds {
			return TimelineResult{}, fmt.Errorf("request times must be 0..%d seconds, got %d", maxSeconds, r.At)
		}
		if len(r.Headers) > maxHeaders {
			return TimelineResult{}, fmt.Errorf("at most %d headers per request", maxHeaders)
		}
		for k, v := range r.Headers {
			if !slices.Contains(requestHeaders, http.CanonicalHeaderKey(k)) {
				return TimelineResult{}, fmt.Errorf("request header %q is not supported; use one of %s", k, strings.Join(requestHeaders, ", "))
			}
			if len(v) > maxValue {
				return TimelineResult{}, fmt.Errorf("request header %s is over %d bytes", k, maxValue)
			}
		}
	}
	reqs := slices.Clone(p.Requests)
	slices.SortStableFunc(reqs, func(a, b Request) int { return cmp.Compare(a.At, b.At) })

	now := epoch
	clock := func() time.Time { return now }
	org := &origin{cfg: p.Origin, now: clock}
	st, err := memstore.New(memstore.Config{
		Capacity:       config.DefaultMemBytes,
		Shards:         config.DefaultMemShards,
		MaxObjectBytes: config.DefaultMemMaxObjectBytes,
		MaxVariants:    config.DefaultMaxVariants,
	})
	if err != nil {
		return TimelineResult{}, err
	}
	defer st.Close()
	eng, err := cache.New(cache.Config{
		Policy: rfc9111.Policy{
			HeuristicFraction: config.DefaultHeuristicFraction,
			HeuristicMax:      config.DefaultHeuristicMax,
		},
		MaxObjectBytes:      config.DefaultMaxObjectBytes,
		MaxVariants:         config.DefaultMaxVariants,
		OrphanFetch:         true,
		OrphanMaxBytes:      config.DefaultOrphanMaxBytes,
		TagHeaders:          strings.Split(config.DefaultTagHeaders, ","),
		JanitorInterval:     config.DefaultJanitorInterval,
		CollapseLeadTimeout: config.DefaultCollapseLeadTimeout,
		CollapseStreamBytes: config.DefaultCollapseStreamBytes,
		RangeFill:           true,
		Now:                 clock,
	}, st, org, nopRecorder{}, slog.New(slog.DiscardHandler))
	if err != nil {
		return TimelineResult{}, err
	}
	defer eng.Stop()
	h := eng.Handler()

	var out TimelineResult
	for _, r := range reqs {
		now = epoch.Add(time.Duration(r.At) * time.Second)
		org.calls = []string{}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, demoURL, http.NoBody)
		if err != nil {
			return TimelineResult{}, err
		}
		for k, v := range r.Headers {
			req.Header.Set(k, v)
		}
		w := &recorder{header: http.Header{}}
		h.ServeHTTP(w, req)
		if w.status == 0 {
			w.status = http.StatusOK
		}
		// A stale-while-revalidate hit refreshes in the background; finish
		// it before the clock moves so every run is deterministic.
		if err := eng.Drain(context.Background()); err != nil {
			return TimelineResult{}, err
		}
		out.Steps = append(out.Steps, Step{
			At:       r.At,
			Status:   w.status,
			XCache:   w.header.Get(cache.HeaderXCache),
			Reason:   w.header.Get(cache.HeaderXCacheReason),
			Age:      w.header.Get("Age"),
			Body:     w.body.String(),
			Upstream: org.calls,
		})
	}
	return out, nil
}

func (o Origin) validate() error {
	for name, v := range map[string]string{"Cache-Control": o.CacheControl, "ETag": o.ETag, "Vary": o.Vary, "Age": o.Age} {
		if len(v) > maxValue {
			return fmt.Errorf("origin %s is over %d bytes", name, maxValue)
		}
	}
	if o.ETag != "" && !entityTag(o.ETag) {
		return fmt.Errorf(`origin ETag must be a quoted entity-tag such as "v1" or W/"v1", got %s`, o.ETag)
	}
	for name, v := range map[string]*int64{"expiresIn": o.ExpiresIn, "lastModifiedAgo": o.LastModifiedAgo} {
		if v != nil && (*v < -maxSeconds || *v > maxSeconds) {
			return fmt.Errorf("origin %s must be -%d..%d seconds, got %d", name, maxSeconds, maxSeconds, *v)
		}
	}
	return nil
}

// entityTag reports whether s is an RFC 9110 entity-tag: optional W/, then
// a quoted string of etagc (%x21 / %x23-7E).
func entityTag(s string) bool {
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for _, c := range []byte(s[1 : len(s)-1]) {
		if c < 0x21 || c == '"' || c > 0x7e {
			return false
		}
	}
	return true
}

// origin is the scripted upstream: a 200 with the configured headers and a
// body naming its version, or a 304 when rfc9111.NotModified says the
// request's validators match the unchanged resource.
type origin struct {
	cfg     Origin
	now     func() time.Time
	version int
	calls   []string
}

func (o *origin) Do(_ context.Context, r *http.Request) (*http.Response, error) {
	date := o.now()
	h := http.Header{}
	h.Set("Date", date.Format(http.TimeFormat))
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	set("Cache-Control", o.cfg.CacheControl)
	set("ETag", o.cfg.ETag)
	set("Vary", o.cfg.Vary)
	set("Age", o.cfg.Age)
	if o.cfg.ExpiresIn != nil {
		h.Set("Expires", date.Add(time.Duration(*o.cfg.ExpiresIn)*time.Second).Format(http.TimeFormat))
	}
	if o.cfg.LastModifiedAgo != nil {
		h.Set("Last-Modified", epoch.Add(-time.Duration(*o.cfg.LastModifiedAgo)*time.Second).Format(http.TimeFormat))
	}

	label := "GET"
	inm, ims := r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
	if inm != "" || ims != "" {
		label = "conditional GET"
	}
	if rfc9111.NotModified(r.Header, h) {
		o.calls = append(o.calls, label+" → 304")
		return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: http.NoBody, Request: r}, nil
	}
	o.version++
	body := "version " + strconv.Itoa(o.version)
	h.Set("Content-Type", "text/plain")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	o.calls = append(o.calls, label+" → 200")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}, nil
}

// recorder is a minimal http.ResponseWriter.
type recorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *recorder) Header() http.Header { return w.header }

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *recorder) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

type nopRecorder struct{}

func (nopRecorder) Request(string, int, time.Duration, time.Duration, int64) {}
func (nopRecorder) Origin(int, time.Duration, error)                         {}
func (nopRecorder) Collapsed(int)                                            {}
func (nopRecorder) StoreGauges(map[string]store.Stats)                       {}
func (nopRecorder) Purged(string, int)                                       {}
func (nopRecorder) VariantEvicted()                                          {}
