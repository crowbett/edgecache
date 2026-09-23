package cache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

// request is the per-request state of the proxy handler; every path fills
// outcome/reason/age and finish emits the one Recorder call and log line.
type request struct {
	c     *Cache
	w     *responseWriter
	r     *http.Request
	rd    rfc9111.ReqDirectives
	start time.Time

	u        *url.URL // absolute form of r.URL (scheme + host filled in)
	k0       cachekey.Key
	variants []store.Variant

	outcome   string
	reason    rfc9111.Reason
	age       time.Duration
	collapsed bool
	waiters   int
	key       cachekey.Key
	hasKey    bool
}

// stale is a stored entry that may still serve: with a conditional request
// (cond) when it has a validator, or on origin failure inside stale-if-error.
// e is nil for background refreshes (no client to serve).
type stale struct {
	e    *store.Entry
	key  cachekey.Key
	meta store.Meta
	cond bool
}

// serveHTTP is the data flow of docs/PLAN.md.
func (c *Cache) serveHTTP(w http.ResponseWriter, r *http.Request) {
	rq := &request{
		c:       c,
		w:       &responseWriter{ResponseWriter: w, now: c.now},
		r:       r,
		start:   c.now(),
		outcome: OutcomeError,
	}
	defer rq.finish()
	rq.rd = rfc9111.ParseRequest(r.Header)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		status, hdr := rq.forward(OutcomeBypass, rfc9111.ReasonUnsafeMethod)
		if status > 0 && status < http.StatusBadRequest {
			rq.invalidateFor(hdr)
		}
		return
	default:
		rq.forward(OutcomeBypass, rfc9111.ReasonMethod)
		return
	}
	if rq.rd.NoStore {
		rq.forward(OutcomeBypass, rfc9111.ReasonReqNoStore)
		return
	}

	rq.u = c.absoluteURL(r)
	rq.k0 = cachekey.Primary(http.MethodGet, rq.u)
	if e, key := rq.lookup(); e != nil {
		rq.serveStored(e, key)
		return
	}
	if rq.rd.OnlyIfCached {
		rq.gatewayTimeout(rfc9111.ReasonOnlyIfCached)
		return
	}
	if r.Header.Get("Range") != "" {
		rq.forward(OutcomeBypass, rfc9111.ReasonRange)
		return
	}
	if r.Method == http.MethodHead {
		// A HEAD miss is forwarded as is: there is no body to store.
		rq.forward(OutcomeMiss, rfc9111.ReasonNone)
		return
	}
	rq.miss(nil)
}

// lookup walks the variants recorded under the primary key and returns the
// first held entry whose Vary-selected key matches this request.
func (rq *request) lookup() (*store.Entry, cachekey.Key) {
	c, ctx := rq.c, rq.r.Context()
	vs, err := c.store.Variants(ctx, rq.k0)
	if err != nil && !errors.Is(err, store.ErrMiss) {
		c.log.Warn("cache: variants", slog.String("primary", rq.k0.String()), slog.Any("err", err))
	}
	rq.variants = vs
	for _, v := range vs {
		k := keyFor(rq.k0, v.Vary, rq.r.Header)
		if k != v.Key {
			continue
		}
		e, err := c.store.Get(ctx, k)
		if err == nil {
			return e, k
		}
		if !errors.Is(err, store.ErrMiss) {
			c.log.Warn("cache: get", slog.String("key", k.String()), slog.Any("err", err))
		}
	}
	if len(vs) == 0 {
		// No variant list (fresh process over a recovered disk tier): the
		// no-vary entry lives under the primary key itself.
		if e, err := c.store.Get(ctx, rq.k0); err == nil {
			return e, rq.k0
		}
	}
	return nil, cachekey.Key{}
}

// serveStored applies Evaluate to a held entry and takes the matching path. It
// owns the entry's release.
func (rq *request) serveStored(e *store.Entry, key cachekey.Key) {
	c := rq.c
	rq.key, rq.hasKey = key, true
	meta := e.Meta
	sd := rfc9111.ParseResponse(meta.Header)
	d := c.cfg.Policy.Evaluate(c.now(), meta.Resp, sd, rq.rd)
	rq.reason = d.Reason
	switch d.Action {
	case rfc9111.Fresh:
		outcome := OutcomeHit
		if d.Reason == rfc9111.ReasonMaxStale {
			outcome = OutcomeStale
		}
		rq.serveEntry(meta, e, outcome, d.Age)
		e.Release()
	case rfc9111.StaleWhileRevalidate:
		rq.serveEntry(meta, e, OutcomeStale, d.Age)
		c.startBackground(rq, key, meta)
		e.Release()
	case rfc9111.Revalidate, rfc9111.Stale:
		if rq.r.Header.Get("Range") != "" || rq.r.Method == http.MethodHead {
			// Neither path can produce a full body to store; go to the origin uncached.
			e.Release()
			rq.forward(OutcomeBypass, d.Reason)
			return
		}
		rq.miss(&stale{e: e, key: key, meta: meta, cond: d.Action == rfc9111.Revalidate})
	case rfc9111.Bypass:
		e.Release()
		rq.forward(OutcomeBypass, d.Reason)
	case rfc9111.GatewayTimeout:
		e.Release()
		rq.gatewayTimeout(d.Reason)
	default:
		e.Release()
		rq.forward(OutcomeBypass, d.Reason)
	}
}

// miss fetches through the collapse group: one leader per key runs the origin
// exchange on the detached root context and streams to its own client; the
// waiters re-read the committed entry. st (may be nil) is a stale entry the
// fetch can validate or fall back to; miss releases it.
func (rq *request) miss(st *stale) {
	c := rq.c
	if st != nil {
		defer st.e.Release()
	}
	led := false
	res, _, waiters, err := c.group.Do(rq.r.Context(), rq.collapseKey(st), func(ctx context.Context) (fetchResult, error) {
		led = true
		return c.fetchInto(ctx, rq, st), nil
	})
	if led {
		rq.waiters = waiters
		if waiters > 0 {
			c.rec.Collapsed(waiters)
		}
		return
	}
	rq.collapsed = true
	if err != nil {
		// This caller stopped waiting (its client left or the leader budget
		// ran out); nothing was written to it.
		rq.reason = rfc9111.ReasonNone
		return
	}
	if res.stored {
		k := keyFor(rq.k0, res.vary, rq.r.Header)
		if k == res.key {
			if e, err := c.store.Get(rq.r.Context(), k); err == nil && rq.serveFresh(e, k) {
				return
			}
		}
	}
	if res.err != nil || res.status >= http.StatusInternalServerError {
		if rq.serveStaleIfError(st, res.status, res.err) {
			return
		}
	}
	// The leader's response does not serve this caller (Vary mismatch,
	// not storable, or failed): fetch directly, still on a detached context.
	ctx, cancel := c.leadCtx()
	defer cancel()
	c.fetchInto(ctx, rq, st)
}

// serveFresh serves a held entry when Evaluate allows it without an origin
// round trip; it releases e either way and reports whether it served.
func (rq *request) serveFresh(e *store.Entry, key cachekey.Key) bool {
	defer e.Release()
	c := rq.c
	meta := e.Meta
	d := c.cfg.Policy.Evaluate(c.now(), meta.Resp, rfc9111.ParseResponse(meta.Header), rq.rd)
	switch d.Action {
	case rfc9111.Fresh:
		rq.key, rq.hasKey, rq.reason = key, true, d.Reason
		outcome := OutcomeHit
		if d.Reason == rfc9111.ReasonMaxStale {
			outcome = OutcomeStale
		}
		rq.serveEntry(meta, e, outcome, d.Age)
		return true
	case rfc9111.StaleWhileRevalidate:
		rq.key, rq.hasKey, rq.reason = key, true, d.Reason
		rq.serveEntry(meta, e, OutcomeStale, d.Age)
		c.startBackground(rq, key, meta)
		return true
	case rfc9111.Stale, rfc9111.Revalidate, rfc9111.Bypass, rfc9111.GatewayTimeout:
		return false
	default:
		return false
	}
}

// collapseKey picks the singleflight key: the stale entry's key when
// revalidating, else the secondary key under the most recent known Vary, else
// the primary key.
func (rq *request) collapseKey(st *stale) string {
	if st != nil {
		return st.key.String()
	}
	if len(rq.variants) > 0 {
		return keyFor(rq.k0, rq.variants[0].Vary, rq.r.Header).String()
	}
	return rq.k0.String()
}

// serveStaleIfError answers from the stale entry when RFC 5861 §4 allows it.
func (rq *request) serveStaleIfError(st *stale, status int, err error) bool {
	if st == nil || st.e == nil {
		return false
	}
	c := rq.c
	now := c.now()
	sd := rfc9111.ParseResponse(st.meta.Header)
	ok, reason := c.cfg.Policy.StaleIfError(now, st.meta.Resp, sd, rq.rd)
	if !ok {
		return false
	}
	c.log.Debug("cache: stale-if-error", slog.String("key", st.key.String()), slog.Int("origin_status", status), slog.Any("err", err))
	rq.key, rq.hasKey, rq.reason = st.key, true, reason
	if reason == rfc9111.ReasonNone {
		rq.reason = rfc9111.ReasonSIE
	}
	rq.serveEntry(st.meta, st.e, OutcomeStale, rfc9111.Age(st.meta.Resp, now))
	return true
}

// startBackground launches one revalidation per key (TryStart dedupes) on the
// root context; Drain waits for it.
func (c *Cache) startBackground(rq *request, key cachekey.Key, meta store.Meta) {
	done, ok := c.group.TryStart(key.String())
	if !ok {
		return
	}
	up := rq.r.Clone(c.root)
	up.Body, up.ContentLength = http.NoBody, 0
	up.Method = http.MethodGet
	for _, h := range []string{"Cache-Control", "Pragma", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "Range"} {
		up.Header.Del(h)
	}
	bgrq := &request{c: c, r: up, u: rq.u, k0: rq.k0, variants: rq.variants, start: c.now()}
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer done()
		ctx, cancel := c.leadCtx()
		defer cancel()
		res := c.fetchInto(ctx, bgrq, &stale{key: key, meta: meta, cond: rfc9111.HasValidator(meta.Header)})
		if res.err != nil {
			c.log.Warn("cache: background revalidation failed", slog.String("key", key.String()), slog.Any("err", res.err))
			return
		}
		c.log.Debug("cache: background revalidation", slog.String("key", key.String()), slog.Int("status", res.status), slog.Bool("stored", res.stored))
	}()
}

// serveEntry writes a stored response: its headers, Age, X-Cache, then the
// body from e.Open() (none for HEAD). A client conditional that matches
// answers 304; a Range on a seekable body goes through http.ServeContent.
func (rq *request) serveEntry(meta store.Meta, e *store.Entry, outcome string, age time.Duration) {
	c, w, r := rq.c, rq.w, rq.r
	rq.outcome, rq.age = outcome, age
	h := w.Header()
	for k, vv := range meta.Header {
		if k == "Age" {
			continue
		}
		h[k] = vv
	}
	h.Set("Age", strconv.FormatInt(int64(age/time.Second), 10))
	h.Set(HeaderXCache, strings.ToUpper(outcome))
	if outcome == OutcomeStale && rq.reason != rfc9111.ReasonNone {
		h.Set(HeaderXCacheReason, string(rq.reason))
	}
	if c.cfg.Debug && rq.hasKey {
		h.Set(HeaderXCacheKey, rq.key.String())
	}
	if notModified(r, meta) {
		h.Del("Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	withBody := bodyAllowed(meta.Status)
	if withBody {
		h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	if withBody && meta.Status == http.StatusOK && r.Header.Get("Range") != "" {
		if rc, err := e.Open(); err == nil {
			if rs, ok := rc.(io.ReadSeeker); ok {
				defer rc.Close()
				h.Del("Content-Length")
				http.ServeContent(w, r, "", meta.LastModified, rs)
				return
			}
			_ = rc.Close()
		}
	}
	w.WriteHeader(meta.Status)
	if !withBody || r.Method == http.MethodHead || meta.Size == 0 {
		return
	}
	rc, err := e.Open()
	if err != nil {
		c.log.Warn("cache: open body", slog.String("key", meta.URL), slog.Any("err", err))
		return
	}
	defer rc.Close()
	buf := c.getBuf()
	defer c.putBuf(buf)
	if _, err := io.CopyBuffer(struct{ io.Writer }{w}, rc, *buf); err != nil {
		c.log.Debug("cache: client write", slog.Any("err", err))
	}
}

// forward passes the request to the origin uncached and streams the answer
// back with X-Cache set to label; it returns the origin status (0 on failure)
// and headers.
func (rq *request) forward(label string, reason rfc9111.Reason) (int, http.Header) {
	c, w, r := rq.c, rq.w, rq.r
	rq.reason = reason
	up := r.Clone(r.Context())
	t0 := c.now()
	resp, err := c.fetch.Do(r.Context(), up)
	d := c.now().Sub(t0)
	if err != nil {
		c.rec.Origin(0, d, err)
		rq.gatewayError(err)
		return 0, nil
	}
	c.rec.Origin(resp.StatusCode, d, nil)
	defer resp.Body.Close()
	hdr := rfc9111.StripHopByHop(resp.Header)
	h := w.Header()
	for k, vv := range hdr {
		h[k] = vv
	}
	h.Set(HeaderXCache, strings.ToUpper(label))
	if reason != rfc9111.ReasonNone {
		h.Set(HeaderXCacheReason, string(reason))
	}
	rq.outcome = label
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || !bodyAllowed(resp.StatusCode) {
		return resp.StatusCode, hdr
	}
	buf := c.getBuf()
	defer c.putBuf(buf)
	if _, err := io.CopyBuffer(struct{ io.Writer }{w}, resp.Body, *buf); err != nil {
		c.log.Debug("cache: forward copy", slog.Any("err", err))
	}
	return resp.StatusCode, hdr
}

// invalidateFor drops the request URL's entries after a successful unsafe
// method, plus Location / Content-Location targets on the same host (§4.4).
func (rq *request) invalidateFor(hdr http.Header) {
	c, r := rq.c, rq.r
	ctx := r.Context()
	base := c.absoluteURL(r)
	c.invalidate(ctx, base)
	for _, name := range []string{"Location", "Content-Location"} {
		v := hdr.Get(name)
		if v == "" {
			continue
		}
		u, err := base.Parse(v)
		if err != nil || !strings.EqualFold(u.Host, base.Host) {
			continue
		}
		c.invalidate(ctx, u)
	}
}

// absoluteURL is r.URL with scheme and host filled in from the request
// (TLS / X-Forwarded-Proto, Host), the same form server.requestURL builds for
// PURGE so both derive the same primary key. The scheme is part of the cache
// key (RFC 9111 §2), so X-Forwarded-Proto counts only from a trusted proxy:
// from anyone else it would let a plain-HTTP client write https entries.
func (c *Cache) absoluteURL(r *http.Request) *url.URL {
	if r.URL.IsAbs() {
		return r.URL
	}
	u := *r.URL
	u.Scheme = "http"
	if r.TLS != nil {
		u.Scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); (p == "https" || p == "http") && c.cfg.TrustedProxies.Trusts(r.RemoteAddr) {
		u.Scheme = p
	}
	u.Host = r.Host
	return &u
}

// gatewayTimeout answers 504 without touching the origin (only-if-cached).
func (rq *request) gatewayTimeout(reason rfc9111.Reason) {
	rq.outcome, rq.reason = OutcomeMiss, reason
	rq.w.Header().Set(HeaderXCache, "MISS")
	rq.w.Header().Set(HeaderXCacheReason, string(reason))
	writeJSONError(rq.w, http.StatusGatewayTimeout, "gateway timeout")
}

// gatewayError maps an origin failure to 502 (unreachable) or 504 (budget).
func (rq *request) gatewayError(err error) {
	rq.outcome = OutcomeError
	status, msg := http.StatusBadGateway, "bad gateway"
	if errors.Is(err, context.DeadlineExceeded) {
		status, msg = http.StatusGatewayTimeout, "gateway timeout"
	}
	rq.c.log.Warn("cache: origin", slog.String("method", rq.r.Method), slog.String("path", rq.r.URL.Path), slog.Any("err", err))
	if rq.w.wroteHeader {
		return
	}
	rq.w.Header().Set(HeaderXCache, "MISS")
	writeJSONError(rq.w, status, msg)
}

// finish emits the one Recorder.Request and log line every path ends in.
func (rq *request) finish() {
	c, w := rq.c, rq.w
	end := c.now()
	total := end.Sub(rq.start)
	var ttfb time.Duration
	if w.wroteHeader {
		ttfb = w.first.Sub(rq.start)
	}
	c.rec.Request(rq.outcome, w.status, ttfb, total, w.bytes)
	key := ""
	if rq.hasKey {
		key = rq.key.String()
	}
	level := slog.LevelDebug
	if rq.outcome == OutcomeError {
		level = slog.LevelInfo
	}
	c.log.Log(rq.r.Context(), level, "cache: request",
		slog.String("outcome", rq.outcome),
		slog.String("reason", string(rq.reason)),
		slog.Int("status", w.status),
		slog.Int64("age_s", int64(rq.age/time.Second)),
		slog.Int64("ttfb_ms", ttfb.Milliseconds()),
		slog.Int64("dur_ms", total.Milliseconds()),
		slog.Int64("bytes", w.bytes),
		slog.Bool("collapsed", rq.collapsed),
		slog.Int("waiters", rq.waiters),
		slog.String("key", key),
		slog.String("request_id", w.Header().Get("X-Request-Id")),
	)
}

// responseWriter records status, byte count and the time of the first write.
type responseWriter struct {
	http.ResponseWriter
	now         func() time.Time
	status      int
	bytes       int64
	first       time.Time
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.first = w.now()
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports it.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// writeJSONError writes a stable JSON error body.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// bodyAllowed reports whether a response with status carries a body.
func bodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}

// notModified evaluates the client's own If-None-Match / If-Modified-Since
// against the stored entry (RFC 9110 §13.1.2, §13.1.3; weak comparison).
func notModified(r *http.Request, meta store.Meta) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if meta.Status != http.StatusOK {
		return false
	}
	if inm := r.Header.Values("If-None-Match"); len(inm) > 0 {
		etag := meta.ETag
		if etag == "" {
			etag = meta.Header.Get("ETag")
		}
		for _, v := range inm {
			for tag := range strings.SplitSeq(v, ",") {
				tag = strings.TrimSpace(tag)
				if tag == "*" || (etag != "" && weakETagEqual(tag, etag)) {
					return true
				}
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !meta.LastModified.IsZero() {
		if t, ok := rfc9111.ParseDate(ims, time.Time{}); ok && !meta.LastModified.Truncate(time.Second).After(t) {
			return true
		}
	}
	return false
}

// weakETagEqual compares two entity tags ignoring the W/ prefix.
func weakETagEqual(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}
