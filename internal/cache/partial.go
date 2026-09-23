package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

// This file holds the partial-content and HEAD paths: byte ranges cut from a
// stored full 200 (RFC 9110 §14), the background fill a range miss starts, and
// the RFC 9111 §4.3.5 freshening of a stored GET by a HEAD response.

// serveRange answers a GET carrying Range from a stored 200 whose headers the
// caller already copied to the response. It reports false when the Range must
// be ignored and the full 200 sent instead: If-Range failed (RFC 9110
// §13.1.5), the header is malformed, uses another unit, asks for too many or
// too many bytes (§14.2), or the body cannot be opened. Unsatisfiable ranges
// get 416 with "Content-Range: bytes */size" (§15.5.17); one range gets a
// single-part 206 (§15.3.7.1), several get multipart/byteranges (§14.6).
func (rq *request) serveRange(meta store.Meta, e *store.Entry) bool {
	r, w := rq.r, rq.w
	if meta.Size <= 0 {
		return false
	}
	if !rfc9111.IfRange(r.Header.Get("If-Range"), meta.Header.Get("ETag"), meta.Header.Get("Last-Modified")) {
		return false
	}
	ranges, err := rfc9111.ParseRange(r.Header.Get("Range"), meta.Size)
	h := w.Header()
	switch {
	case errors.Is(err, rfc9111.ErrRangeUnsatisfiable):
		h.Del("Content-Type")
		h.Del("Content-Encoding")
		h.Set("Content-Range", rfc9111.UnsatisfiedRange(meta.Size))
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return true
	case err != nil:
		return false
	}
	if len(ranges) > 1 && meta.Header.Get("Content-Encoding") != "" {
		// A multipart body under the stored Content-Encoding would claim the
		// whole multipart payload is encoded; sending the 200 is always allowed.
		return false
	}
	rc, err := e.Open()
	if err != nil {
		rq.c.log.Warn("cache: open body for range", slog.String("key", meta.URL), slog.Any("err", err))
		return false
	}
	defer rc.Close()
	src := rangeSource{e: e, rc: rc}
	buf := rq.c.getBuf()
	defer rq.c.putBuf(buf)

	if len(ranges) == 1 {
		br := ranges[0]
		h.Set("Content-Range", br.ContentRange(meta.Size))
		h.Set("Content-Length", strconv.FormatInt(br.Length, 10))
		w.WriteHeader(http.StatusPartialContent)
		if err := src.copy(w, br, *buf); err != nil {
			rq.c.log.Debug("cache: range write", slog.Any("err", err))
		}
		return true
	}

	boundary := rand.Text()
	ctype := meta.Header.Get("Content-Type")
	parts := make([]string, len(ranges))
	length := int64(len(multipartEnd(boundary)))
	for i, br := range ranges {
		parts[i] = partHeader(boundary, ctype, br.ContentRange(meta.Size), i == 0)
		length += int64(len(parts[i])) + br.Length
	}
	h.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	for i, br := range ranges {
		if _, err := io.WriteString(w, parts[i]); err != nil {
			return true
		}
		if err := src.copy(w, br, *buf); err != nil {
			rq.c.log.Debug("cache: range write", slog.Any("err", err))
			return true
		}
	}
	_, _ = io.WriteString(w, multipartEnd(boundary))
	return true
}

// partHeader is one multipart/byteranges part's delimiter and header block
// (RFC 9110 §14.6, RFC 2046 §5.1.1): CRLF before every delimiter but the first.
func partHeader(boundary, ctype, contentRange string, first bool) string {
	s := "--" + boundary + "\r\n"
	if !first {
		s = "\r\n" + s
	}
	if ctype != "" {
		s += "Content-Type: " + ctype + "\r\n"
	}
	return s + "Content-Range: " + contentRange + "\r\n\r\n"
}

// multipartEnd is the close delimiter.
func multipartEnd(boundary string) string { return "\r\n--" + boundary + "--\r\n" }

// rangeSource reads byte ranges of a stored body: through io.ReaderAt when the
// tier's reader has it (bytes.Reader, os.File), else by reopening the entry
// and skipping to the start.
type rangeSource struct {
	e  *store.Entry
	rc io.ReadCloser
}

func (s rangeSource) copy(w io.Writer, br rfc9111.ByteRange, buf []byte) error {
	dst := struct{ io.Writer }{w} // hide ReadFrom so buf is used
	if ra, ok := s.rc.(io.ReaderAt); ok {
		_, err := io.CopyBuffer(dst, io.NewSectionReader(ra, br.Start, br.Length), buf)
		return err
	}
	rc, err := s.e.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := io.CopyN(io.Discard, rc, br.Start); err != nil {
		return err
	}
	_, err = io.CopyBuffer(dst, io.LimitReader(rc, br.Length), buf)
	return err
}

// worthFilling decides whether a range miss's pass-through answer shows a
// representation the store would keep, judged by the same Storable rule a
// plain GET gets, with the partial-content markers taken off: a 206 or a 200
// that is not no-store/private, has no Vary: *, and whose complete length
// (Content-Range "/N") fits MaxObjectBytes. Anything else would make every
// range miss cost a second, useless full fetch.
func (c *Cache) worthFilling(rq *request, up upstream) bool {
	if up.status != http.StatusOK && up.status != http.StatusPartialContent {
		return false
	}
	h := up.header.Clone()
	cr := h.Get("Content-Range")
	h.Del("Content-Range")
	res := rfc9111.Resp{Status: http.StatusOK, Header: h, RequestTime: up.reqTime, ResponseTime: up.respTime}
	fill := rq.r.Clone(rq.r.Context())
	fill.Header.Del("Range")
	if ok, _ := c.cfg.Policy.Storable(fill, rq.rd, res, rfc9111.ParseResponse(h)); !ok {
		return false
	}
	if _, err := cachekey.NormalizeVary(h.Values("Vary")); err != nil {
		return false
	}
	if c.cfg.MaxObjectBytes > 0 && cr != "" {
		if total, ok := completeLength(cr); ok && total > c.cfg.MaxObjectBytes {
			return false
		}
	}
	return true
}

// completeLength reads the complete-length of "bytes a-b/N"; "*" or anything
// malformed reports false.
func completeLength(contentRange string) (int64, bool) {
	for i := len(contentRange) - 1; i >= 0; i-- {
		if contentRange[i] == '/' {
			n, err := strconv.ParseInt(contentRange[i+1:], 10, 64)
			return n, err == nil && n >= 0
		}
	}
	return 0, false
}

// startFill runs one background full GET for a range miss (TryStart dedupes
// per key) through the collapse group, so a plain GET miss arriving meanwhile
// waits for this fetch instead of starting its own. It has no client: the
// orphan budget does not apply, MaxObjectBytes does.
func (c *Cache) startFill(rq *request) {
	bgrq := c.backgroundRequest(rq)
	key := bgrq.collapseKey(nil)
	done, ok := c.group.TryStart("fill:" + key)
	if !ok {
		return
	}
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer done()
		res, _, _, err := c.group.Do(c.root, key, func(ctx context.Context) (fetchResult, error) {
			return c.fetchInto(ctx, bgrq, nil), nil
		})
		if err == nil {
			err = res.err
		}
		if err != nil {
			c.log.Warn("cache: range fill failed", slog.String("url", bgrq.u.Redacted()), slog.Any("err", err))
			return
		}
		c.log.Debug("cache: range fill", slog.String("key", res.key.String()), slog.Int("status", res.status), slog.Bool("stored", res.stored))
	}()
}

// freshenFromHead applies RFC 9111 §4.3.5 after a HEAD for a stale entry
// came back 200: when the validators (and Content-Length) match, the stored
// GET takes the HEAD's header fields as a 304 would give them (§3.2, §4.3.4)
// and its freshness restarts; otherwise it is marked stale (lifetime 0) so
// the next GET revalidates or refetches. A HEAD saying the response may no
// longer be stored (no-store, private, ...) drops the entry, as a GET would.
func (c *Cache) freshenFromHead(rq *request, key cachekey.Key, stored store.Meta, up upstream) {
	ctx := rq.r.Context()
	res := rfc9111.Resp{Status: http.StatusOK, Header: up.header, RequestTime: up.reqTime, ResponseTime: up.respTime}
	if ok, _ := c.cfg.Policy.Storable(rq.r, rq.rd, res, rfc9111.ParseResponse(up.header)); !ok {
		if _, err := c.store.Delete(ctx, key); err != nil {
			c.log.Debug("cache: head drop", slog.String("key", key.String()), slog.Any("err", err))
		}
		c.tags.Remove(key)
		return
	}
	nm := stored
	if rfc9111.HeadMatches(stored.Header, stored.Size, up.header) {
		nm = c.refreshedMeta(stored, res)
	} else {
		nm.Lifetime, nm.Heuristic = 0, false
	}
	if err := c.store.UpdateMeta(ctx, key, nm); err != nil {
		c.log.Warn("cache: head update meta", slog.String("key", key.String()), slog.Any("err", err))
		return
	}
	c.tags.Add(key, nm.URL, nm.Tags)
}
