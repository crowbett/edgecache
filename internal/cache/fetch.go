package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

// ReasonStoreError is the X-Cache-Reason when the store refused the writer.
const ReasonStoreError rfc9111.Reason = "store-error"

// fetchResult is what a collapse leader publishes to its waiters: whether an
// entry was committed (or refreshed) and under which key and Vary, the origin
// status, and the transport error if the exchange failed.
type fetchResult struct {
	stored bool
	key    cachekey.Key
	vary   []string
	status int
	err    error
}

// fetchInto runs one origin exchange for rq on ctx (a detached context) and,
// when rq has a client (rq.w != nil), streams the answer to it while teeing
// the body into the store. st is a stale entry to validate (st.cond) or fall
// back to on failure; it may be nil. Background refreshes call it with no
// client and a stale without an entry.
func (c *Cache) fetchInto(ctx context.Context, rq *request, st *stale) fetchResult {
	r := rq.r
	up := r.Clone(ctx)
	up.Body, up.ContentLength = http.NoBody, 0
	if st != nil && st.cond {
		rfc9111.ConditionalRequest(up.Header, st.meta.Resp)
	}
	t0 := c.now()
	resp, err := c.fetch.Do(ctx, up)
	t1 := c.now()
	if err != nil {
		c.rec.Origin(0, t1.Sub(t0), err)
		if rq.w != nil && !rq.serveStaleIfError(st, 0, err) {
			rq.gatewayError(err)
		}
		return fetchResult{err: err}
	}
	c.rec.Origin(resp.StatusCode, t1.Sub(t0), nil)
	defer resp.Body.Close()

	res := rfc9111.Resp{
		Status:       resp.StatusCode,
		Header:       rfc9111.StripHopByHop(resp.Header),
		RequestTime:  t0,
		ResponseTime: t1,
	}
	if resp.StatusCode == http.StatusNotModified {
		if st != nil && st.cond {
			return c.revalidated(ctx, rq, st, res)
		}
		// The client's own conditional was forwarded: pass the 304 on.
		if rq.w != nil {
			rq.writeUpstream(res, OutcomeMiss, rfc9111.ReasonConditional)
		}
		return fetchResult{status: resp.StatusCode}
	}
	if resp.StatusCode >= http.StatusInternalServerError && st != nil {
		if rq.w == nil {
			// Background refresh: keep the stale entry, try again next time.
			return fetchResult{status: resp.StatusCode}
		}
		if rq.serveStaleIfError(st, resp.StatusCode, nil) {
			return fetchResult{status: resp.StatusCode}
		}
	}

	sd := rfc9111.ParseResponse(res.Header)
	storable, reason := c.cfg.Policy.Storable(r, rq.rd, res, sd)
	var vary []string
	if storable {
		var verr error
		if vary, verr = cachekey.NormalizeVary(res.Header.Values("Vary")); verr != nil {
			storable, reason = false, rfc9111.ReasonVaryStar
		}
	}
	if storable && c.cfg.MaxObjectBytes > 0 && resp.ContentLength > c.cfg.MaxObjectBytes {
		storable, reason = false, rfc9111.ReasonTooLarge
	}

	var (
		wr   store.Writer
		meta store.Meta
		key  cachekey.Key
	)
	if storable {
		meta, key = c.metaFor(rq, res, sd, vary)
		if resp.ContentLength > 0 {
			meta.Size = resp.ContentLength
		}
		w, err := c.store.Writer(ctx, key, meta)
		switch {
		case err == nil:
			wr = w
			defer func() { _ = w.Abort() }() // idempotent after Commit; wr itself may be nil'ed by tee
		case errors.Is(err, store.ErrTooLarge):
			storable, reason = false, rfc9111.ReasonTooLarge
		default:
			c.log.Warn("cache: store writer", slog.String("key", key.String()), slog.Any("err", err))
			storable, reason = false, ReasonStoreError
		}
	}
	if st != nil && !storable && resp.StatusCode < http.StatusInternalServerError {
		// The origin no longer lets this be cached: the stale entry goes (§3).
		if _, err := c.store.Delete(ctx, st.key); err != nil {
			c.log.Debug("cache: drop stale", slog.String("key", st.key.String()), slog.Any("err", err))
		}
		c.tags.Remove(st.key)
	}
	if rq.w != nil {
		if storable {
			rq.key, rq.hasKey = key, true
			rq.writeUpstream(res, OutcomeMiss, rfc9111.ReasonNone)
		} else {
			rq.writeUpstream(res, OutcomeBypass, reason)
		}
	}

	var body io.Reader = resp.Body
	if !bodyAllowed(resp.StatusCode) || r.Method == http.MethodHead {
		body = strings.NewReader("")
	}
	total, wr, rerr := c.tee(body, rq, wr)
	if rerr != nil {
		c.log.Warn("cache: origin body", slog.String("path", r.URL.Path), slog.Int64("bytes", total), slog.Any("err", rerr))
		return fetchResult{status: resp.StatusCode, err: rerr}
	}
	if wr == nil {
		return fetchResult{status: resp.StatusCode, key: key, vary: vary}
	}
	meta.Size = total
	e, err := wr.Commit(meta)
	if err != nil {
		c.log.Warn("cache: commit", slog.String("key", key.String()), slog.Int64("bytes", total), slog.Any("err", err))
		return fetchResult{status: resp.StatusCode, key: key, vary: vary}
	}
	e.Release()
	c.registerVariant(ctx, rq.k0, key, vary, rq.variants, t1)
	c.tags.Add(key, meta.URL, meta.Tags)
	return fetchResult{stored: true, key: key, vary: vary, status: resp.StatusCode}
}

// metaFor builds the Meta the store persists for a storable response and the
// key it goes under.
func (c *Cache) metaFor(rq *request, res rfc9111.Resp, sd rfc9111.RespDirectives, vary []string) (store.Meta, cachekey.Key) {
	stored := res
	stored.Header = rfc9111.StorableHeader(res.Header, sd, res.ResponseTime)
	stored.Lifetime, stored.Heuristic = c.cfg.Policy.Lifetime(stored, sd)
	meta := store.Meta{
		Resp:   stored,
		Method: http.MethodGet,
		URL:    cachekey.Canonical(rq.u),
		ETag:   stored.Header.Get("ETag"),
		Vary:   vary,
		Tags:   c.tagsOf(stored.Header),
	}
	if lm, ok := rfc9111.ParseDate(stored.Header.Get("Last-Modified"), res.ResponseTime); ok {
		meta.LastModified = lm
	}
	return meta, keyFor(rq.k0, vary, rq.r.Header)
}

// revalidated handles a 304 to our own conditional request: merge the headers
// (§4.3.4), refresh the times and lifetime, UpdateMeta, serve REVALIDATED.
func (c *Cache) revalidated(ctx context.Context, rq *request, st *stale, res rfc9111.Resp) fetchResult {
	merged := rfc9111.Merge304(st.meta.Header, res.Header)
	sd := rfc9111.ParseResponse(merged)
	merged = rfc9111.StorableHeader(merged, sd, res.ResponseTime)
	nm := st.meta
	nm.Resp = rfc9111.Resp{
		Status:       st.meta.Status,
		Header:       merged,
		RequestTime:  res.RequestTime,
		ResponseTime: res.ResponseTime,
	}
	nm.Lifetime, nm.Heuristic = c.cfg.Policy.Lifetime(nm.Resp, sd)
	nm.ETag = merged.Get("ETag")
	nm.Tags = c.tagsOf(merged)
	if lm, ok := rfc9111.ParseDate(merged.Get("Last-Modified"), res.ResponseTime); ok {
		nm.LastModified = lm
	}
	if err := c.store.UpdateMeta(ctx, st.key, nm); err != nil {
		c.log.Warn("cache: update meta", slog.String("key", st.key.String()), slog.Any("err", err))
	}
	c.tags.Add(st.key, nm.URL, nm.Tags)
	if rq.w != nil {
		rq.key, rq.hasKey = st.key, true
		rq.serveEntry(nm, st.e, OutcomeRevalidated, rfc9111.Age(nm.Resp, c.now()))
	}
	return fetchResult{stored: true, key: st.key, vary: nm.Vary, status: http.StatusNotModified}
}

// writeUpstream copies an origin response's headers to the client with the
// cache headers and writes the status line.
func (rq *request) writeUpstream(res rfc9111.Resp, label string, reason rfc9111.Reason) {
	h := rq.w.Header()
	for k, vv := range res.Header {
		h[k] = vv
	}
	if label == OutcomeMiss {
		h.Set("Age", "0")
	}
	h.Set(HeaderXCache, strings.ToUpper(label))
	if reason != rfc9111.ReasonNone {
		h.Set(HeaderXCacheReason, string(reason))
	}
	if rq.c.cfg.Debug && rq.hasKey {
		h.Set(HeaderXCacheKey, rq.key.String())
	}
	rq.outcome, rq.reason = label, reason
	rq.w.WriteHeader(res.Status)
}

// tee is the miss copy loop: every chunk goes to the store writer (while one
// is open) and to the client (until it errors or its context ends). After the
// client is gone the store keeps filling up to OrphanMaxBytes (OrphanFetch),
// then the writer is aborted. It returns the bytes read, the writer still open
// (nil once aborted) and the origin read error, if any.
func (c *Cache) tee(body io.Reader, rq *request, wr store.Writer) (int64, store.Writer, error) {
	buf := c.getBuf()
	defer c.putBuf(buf)
	var (
		total      int64
		client     io.Writer
		clientCtx  context.Context
		clientDead = rq.w == nil
	)
	if !clientDead {
		client, clientCtx = rq.w, rq.r.Context()
	}
	for {
		n, rerr := body.Read(*buf)
		if n > 0 {
			chunk := (*buf)[:n]
			total += int64(n)
			if wr != nil {
				if c.cfg.MaxObjectBytes > 0 && total > c.cfg.MaxObjectBytes {
					_ = wr.Abort()
					wr = nil
					c.log.Debug("cache: object over MaxObjectBytes, passthrough", slog.String("path", rq.r.URL.Path))
				} else if _, err := wr.Write(chunk); err != nil {
					_ = wr.Abort()
					wr = nil
					c.log.Warn("cache: store write", slog.String("path", rq.r.URL.Path), slog.Any("err", err))
				}
			}
			if !clientDead {
				if _, err := client.Write(chunk); err != nil || clientCtx.Err() != nil {
					clientDead = true
					c.log.Debug("cache: client gone mid-body", slog.String("path", rq.r.URL.Path), slog.Int64("bytes", total))
				}
			}
			if clientDead {
				if wr == nil {
					return total, nil, nil
				}
				if !c.cfg.OrphanFetch || (c.cfg.OrphanMaxBytes > 0 && total > c.cfg.OrphanMaxBytes) {
					_ = wr.Abort()
					return total, nil, nil
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, wr, nil
			}
			if wr != nil {
				_ = wr.Abort()
			}
			return total, nil, rerr
		}
	}
}
