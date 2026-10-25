package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

// ReasonStoreError is the X-Cache-Reason when the store refused the writer.
const ReasonStoreError rfc9111.Reason = "store-error"

// fetchResult is what a collapse leader publishes to its waiters: whether an
// entry was committed (or refreshed) and under which key and Vary, the origin
// status, and the transport error if the exchange failed. The early result a
// leader publishes while the body is still downloading carries the stream
// instead (stored false, key and vary set).
type fetchResult struct {
	stored bool
	key    cachekey.Key
	vary   []string
	status int
	err    error
	stream *stream
	// truncated: the origin body failed after the status line went out, so
	// the client's response is short (see abortIfTruncated).
	truncated bool
}

// fetchInto runs one origin exchange for rq on ctx (a detached context) and,
// when rq has a client (rq.w != nil), streams the answer to it while teeing
// the body into the store. st is a stale entry to validate (st.cond) or fall
// back to on failure; it may be nil. Background refreshes call it with no
// client and a stale without an entry.
//
// publish is the collapse group's early-result hook (nil outside a leader):
// when the answer is storable and CollapseStreamBytes allows, the body is
// teed into a stream too and published so the waiters serve it while it
// downloads.
func (c *Cache) fetchInto(ctx context.Context, rq *request, st *stale, publish func(fetchResult)) (res fetchResult) {
	var sh *stream
	defer func() {
		// Also on panic: a waiter blocked on the stream must not hang.
		sh.end(errStreamAbandoned) // no-op after a normal end
		sh.settle(res)
	}()
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

	ores := rfc9111.Resp{
		Status:       resp.StatusCode,
		Header:       rfc9111.StripHopByHop(resp.Header),
		RequestTime:  t0,
		ResponseTime: t1,
	}
	if resp.StatusCode == http.StatusNotModified {
		if st != nil && st.cond {
			return c.revalidated(ctx, rq, st, ores)
		}
		// The client's own conditional was forwarded: pass the 304 on.
		if rq.w != nil {
			rq.writeUpstream(ores, OutcomeMiss, rfc9111.ReasonConditional)
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

	sd := rfc9111.ParseResponse(ores.Header)
	storable, reason := c.cfg.Policy.Storable(r, rq.rd, ores, sd)
	var vary []string
	if storable {
		var verr error
		if vary, verr = cachekey.NormalizeVary(ores.Header.Values("Vary")); verr != nil {
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
		meta, key = c.metaFor(rq, ores, sd, vary)
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
			rq.writeUpstream(ores, OutcomeMiss, rfc9111.ReasonNone)
		} else {
			rq.writeUpstream(ores, OutcomeBypass, reason)
		}
	}

	var body io.Reader = resp.Body
	if !bodyAllowed(resp.StatusCode) || r.Method == http.MethodHead {
		body = strings.NewReader("")
	}
	if storable && publish != nil && c.cfg.CollapseStreamBytes > 0 && r.Method == http.MethodGet {
		// Only a storable answer is shared: it is what the waiters would have
		// read from the store after the commit, so nothing private leaks.
		length := resp.ContentLength
		if !bodyAllowed(resp.StatusCode) {
			length = 0
		}
		sh = newStream(meta, length, c.cfg.CollapseStreamBytes)
		publish(fetchResult{key: key, vary: vary, status: resp.StatusCode, stream: sh})
	}
	total, wr, rerr := c.tee(body, rq, wr, sh)
	if rerr != nil {
		c.log.Warn("cache: origin body", slog.String("path", r.URL.Path), slog.Int64("bytes", total), slog.Any("err", rerr))
		return fetchResult{status: resp.StatusCode, err: rerr, truncated: true}
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
	nm := c.refreshedMeta(st.meta, res)
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

// refreshedMeta is stored updated by the header-only response res (a 304 to
// our conditional, or a matching 200 to a HEAD): fields merged per §3.2 /
// §4.3.4, request/response times and lifetime recomputed, validators and tags
// re-read. Status, body size and Vary are the stored ones.
func (c *Cache) refreshedMeta(stored store.Meta, res rfc9111.Resp) store.Meta {
	merged := rfc9111.Merge304(stored.Header, res.Header)
	sd := rfc9111.ParseResponse(merged)
	merged = rfc9111.StorableHeader(merged, sd, res.ResponseTime)
	nm := stored
	nm.Resp = rfc9111.Resp{
		Status:       stored.Status,
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
	return nm
}

// writeUpstream copies an origin response's headers to the client with the
// cache headers and writes the status line.
func (rq *request) writeUpstream(res rfc9111.Resp, label string, reason rfc9111.Reason) {
	h := rq.w.Header()
	for k, vv := range res.Header {
		h[k] = vv
	}
	if label == OutcomeMiss {
		h.Set("Age", strconv.FormatInt(int64(rfc9111.Age(res, rq.c.now())/time.Second), 10))
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
// then the writer is aborted. Every chunk also goes to sh (may be nil); while a
// waiter is reading sh the body is read to the end whatever happened to the
// leader's client and writer. It returns the bytes read, the writer still open
// (nil once aborted) and the origin read error, if any.
func (c *Cache) tee(body io.Reader, rq *request, wr store.Writer, sh *stream) (int64, store.Writer, error) {
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
			sh.append(chunk)
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
				// The orphan budget is for a client that left. A background
				// refresh or range fill never had one; MaxObjectBytes bounds it.
				if wr != nil && rq.w != nil && (!c.cfg.OrphanFetch || (c.cfg.OrphanMaxBytes > 0 && total > c.cfg.OrphanMaxBytes)) {
					_ = wr.Abort()
					wr = nil
				}
				if wr == nil && sh.abandon() {
					return total, nil, nil
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				sh.end(nil)
				return total, wr, nil
			}
			if wr != nil {
				_ = wr.Abort()
			}
			sh.end(rerr)
			return total, nil, rerr
		}
	}
}
