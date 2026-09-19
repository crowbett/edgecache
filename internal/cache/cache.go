// Package cache is the engine: it joins the key derivation, the RFC 9111
// policy, the store and the origin into the catch-all proxy handler (hit, miss,
// revalidate, stale and bypass paths, the tee copy, orphan fetch) plus Purge.
// Its ports (Store, Fetcher, Recorder) are defined here, where they are
// consumed; memstore/diskstore/tiered, *origin.Origin and *metrics.Registry
// satisfy them.
//
// Store, Fetcher and Recorder are frozen at step 1 and change only through the
// integrator.
package cache

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/collapse"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
	"github.com/crowbett/edgecache/internal/tagindex"
)

// Store is dumb keyed blob storage as the engine needs it; every tier satisfies it.
type Store interface {
	Get(ctx context.Context, k cachekey.Key) (*store.Entry, error)
	Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error)
	UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error // 304 merge; body untouched
	Delete(ctx context.Context, keys ...cachekey.Key) (int, error)
	Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error)
	PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error
	Stats() map[string]store.Stats // keyed by tier
	Close() error
}

// Walker is optionally implemented by a Store (disk and tiered do) so New can
// rebuild the tag index from the recovered metas; it is type-asserted, never required.
type Walker interface {
	Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error
}

// Sweeper is optionally implemented by a Store (disk and tiered do); Run calls
// it on every janitor tick.
type Sweeper interface {
	Sweep(ctx context.Context) (int, error)
}

// Fetcher performs the upstream exchange; *origin.Origin satisfies it.
type Fetcher interface {
	Do(ctx context.Context, r *http.Request) (*http.Response, error)
}

// Recorder receives every observable event; *metrics.Registry satisfies it.
// outcome is one of hit|miss|stale|revalidated|bypass|error; kind is url|prefix|tag.
type Recorder interface {
	Request(outcome string, status int, ttfb, total time.Duration, bytes int64)
	Origin(status int, d time.Duration, err error)
	Collapsed(waiters int)
	StoreGauges(map[string]store.Stats)
	Purged(kind string, n int)
	VariantEvicted()
}

// Outcome labels for Recorder.Request and the X-Cache header.
const (
	OutcomeHit         = "hit"
	OutcomeMiss        = "miss"
	OutcomeStale       = "stale"
	OutcomeRevalidated = "revalidated"
	OutcomeBypass      = "bypass"
	OutcomeError       = "error"
)

// Response headers the engine sets.
const (
	HeaderXCache       = "X-Cache"
	HeaderXCacheReason = "X-Cache-Reason"
	HeaderXCacheKey    = "X-Cache-Key"
)

// Purge kinds passed to Recorder.Purged.
const (
	PurgeKindURL    = "url"
	PurgeKindPrefix = "prefix"
	PurgeKindTag    = "tag"
)

// DefaultTagHeaders are read as surrogate keys when Config.TagHeaders is empty.
var DefaultTagHeaders = []string{"Cache-Tag", "Surrogate-Key"}

const (
	defaultJanitorInterval = 30 * time.Second
	copyBufSize            = 32 << 10
)

// Config is the engine's tunable subset of config.Config (main maps it).
type Config struct {
	Policy              rfc9111.Policy
	MaxObjectBytes      int64
	MaxVariants         int
	DefaultSWR          time.Duration
	DefaultSIE          time.Duration
	OrphanFetch         bool
	OrphanMaxBytes      int64
	TagHeaders          []string // canonical header names read as surrogate keys
	JanitorInterval     time.Duration
	CollapseLeadTimeout time.Duration
	PurgeVerbose        bool
	// Debug adds X-Cache-Key to responses.
	Debug bool
	// Now is the engine's clock; nil means time.Now. Tests inject a fixed one.
	Now func() time.Time
}

// PurgeRequest selects entries by exact URL, URL prefix or surrogate tag;
// exactly one field is set.
type PurgeRequest struct {
	URL, Prefix, Tag string
}

// PurgeResult reports how many entries went; Keys is filled only with PURGE_VERBOSE.
type PurgeResult struct {
	Purged int
	Keys   []string
}

// ErrBadPurge is returned by Purge when the request selects nothing or more than one selector.
var ErrBadPurge = errors.New("cache: purge needs exactly one of url, prefix, tag")

// Cache is the engine.
type Cache struct {
	cfg   Config
	store Store
	fetch Fetcher
	rec   Recorder
	log   *slog.Logger

	tags  *tagindex.Index
	group *collapse.Group[fetchResult]
	// root is the detached context every collapse leader and background
	// refresh derives from; stop cancels it (Run's ctx ending, Stop, or a
	// Drain that ran out of time).
	root context.Context //nolint:containedctx // the engine's own root, by design
	stop context.CancelFunc
	bg   sync.WaitGroup

	tagHeaders []string
	warned     sync.Map // primary key -> struct{}: variant-cap warning, once per key
	bufs       sync.Pool
}

// New wires the engine; it rebuilds the tag index when s implements Walker.
func New(cfg Config, s Store, f Fetcher, rec Recorder, log *slog.Logger) (*Cache, error) {
	if s == nil || f == nil || rec == nil {
		return nil, errors.New("cache: store, fetcher and recorder are required")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.JanitorInterval <= 0 {
		cfg.JanitorInterval = defaultJanitorInterval
	}
	// The policy's RFC 5861 defaults come from the engine config so main only maps once.
	if cfg.Policy.DefaultSWR == 0 {
		cfg.Policy.DefaultSWR = cfg.DefaultSWR
	}
	if cfg.Policy.DefaultSIE == 0 {
		cfg.Policy.DefaultSIE = cfg.DefaultSIE
	}
	root, stop := context.WithCancel(context.Background())
	c := &Cache{
		cfg:   cfg,
		store: s,
		fetch: f,
		rec:   rec,
		log:   log,
		tags:  tagindex.NewIndex(),
		group: collapse.New[fetchResult](root, cfg.CollapseLeadTimeout),
		root:  root,
		stop:  stop,
	}
	c.bufs.New = func() any {
		b := make([]byte, copyBufSize)
		return &b
	}
	c.tagHeaders = canonicalNames(cfg.TagHeaders)
	if len(c.tagHeaders) == 0 {
		c.tagHeaders = canonicalNames(DefaultTagHeaders)
	}
	if wk, ok := s.(Walker); ok {
		n := 0
		err := wk.Walk(root, func(k cachekey.Key, m store.Meta) error {
			c.tags.Add(k, m.URL, m.Tags)
			n++
			return nil
		})
		if err != nil {
			// Recovery is best effort: a partial index only narrows prefix/tag purges.
			log.Warn("cache: tag index rebuild incomplete", slog.Int("entries", n), slog.Any("err", err))
		} else {
			log.Info("cache: tag index rebuilt", slog.Int("entries", n))
		}
	}
	return c, nil
}

// Handler returns the catch-all proxy handler. It is mounted without
// http.TimeoutHandler; its budget comes from the origin context.
func (c *Cache) Handler() http.Handler {
	return http.HandlerFunc(c.serveHTTP)
}

// Purge deletes by URL (every variant), prefix or tag and records
// edgecache_purges_total{kind}.
func (c *Cache) Purge(ctx context.Context, req PurgeRequest) (PurgeResult, error) {
	set := 0
	for _, v := range []string{req.URL, req.Prefix, req.Tag} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return PurgeResult{}, ErrBadPurge
	}
	var (
		keys []cachekey.Key
		kind string
	)
	switch {
	case req.URL != "":
		kind = PurgeKindURL
		u, err := url.Parse(req.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return PurgeResult{}, errors.Join(ErrBadPurge, errors.New("cache: purge url needs scheme and host"))
		}
		keys, err = c.keysForURL(ctx, u)
		if err != nil {
			return PurgeResult{}, err
		}
	case req.Prefix != "":
		kind = PurgeKindPrefix
		keys = c.tags.ByPrefix(canonicalPrefix(req.Prefix))
	default:
		kind = PurgeKindTag
		keys = c.tags.ByTag(req.Tag)
	}
	n := 0
	if len(keys) > 0 {
		var err error
		n, err = c.store.Delete(ctx, keys...)
		if err != nil {
			return PurgeResult{}, err
		}
		for _, k := range keys {
			c.tags.Remove(k)
		}
	}
	c.rec.Purged(kind, n)
	res := PurgeResult{Purged: n}
	if c.cfg.PurgeVerbose {
		res.Keys = make([]string, 0, len(keys))
		for _, k := range keys {
			res.Keys = append(res.Keys, k.String())
		}
	}
	c.log.Info("cache: purge", slog.String("kind", kind), slog.Int("selected", len(keys)), slog.Int("purged", n))
	return res, nil
}

// keysForURL is the primary key of u plus every variant key recorded under it.
func (c *Cache) keysForURL(ctx context.Context, u *url.URL) ([]cachekey.Key, error) {
	k0 := cachekey.Primary(http.MethodGet, u)
	vs, err := c.store.Variants(ctx, k0)
	if err != nil && !errors.Is(err, store.ErrMiss) {
		return nil, err
	}
	keys := []cachekey.Key{k0}
	for _, v := range vs {
		if v.Key != k0 {
			keys = append(keys, v.Key)
		}
	}
	return keys, nil
}

// invalidate drops every entry stored for u (RFC 9111 §4.4).
func (c *Cache) invalidate(ctx context.Context, u *url.URL) {
	keys, err := c.keysForURL(ctx, u)
	if err != nil {
		c.log.Warn("cache: invalidate lookup", slog.Any("err", err))
		return
	}
	if _, err := c.store.Delete(ctx, keys...); err != nil {
		c.log.Warn("cache: invalidate delete", slog.Any("err", err))
	}
	for _, k := range keys {
		c.tags.Remove(k)
	}
}

// Run is the janitor: every JanitorInterval it sweeps the store (Sweeper) and
// pushes StoreGauges. ctx is the engine's root: when it ends, Run returns and
// every in-flight collapse leader and background refresh is cancelled.
func (c *Cache) Run(ctx context.Context) {
	defer c.stop()
	c.rec.StoreGauges(c.store.Stats())
	t := time.NewTicker(c.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.janitor(ctx)
		}
	}
}

// janitor is one tick: sweep tombstones, refresh the gauges.
func (c *Cache) janitor(ctx context.Context) {
	if sw, ok := c.store.(Sweeper); ok {
		n, err := sw.Sweep(ctx)
		if err != nil {
			c.log.Warn("cache: sweep", slog.Int("swept", n), slog.Any("err", err))
		} else if n > 0 {
			c.log.Debug("cache: sweep", slog.Int("swept", n))
		}
	}
	c.rec.StoreGauges(c.store.Stats())
}

// Stop cancels the engine's root context: every collapse leader and background
// refresh aborts. Teardown calls it when the proxy drain runs out of time.
func (c *Cache) Stop() { c.stop() }

// InFlight reports collapse leaders currently fetching (for /admin/stats).
func (c *Cache) InFlight() int { return c.group.InFlight() }

// TagIndexLen reports how many keys the surrogate-key index holds (for /admin/stats).
func (c *Cache) TagIndexLen() int { return c.tags.Len() }

// Drain waits for in-flight background revalidations and collapse leaders to
// finish or ctx to end; on ctx ending it cancels them and returns ctx.Err().
func (c *Cache) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		c.stop()
		return ctx.Err()
	}
	if err := c.group.Wait(ctx); err != nil {
		c.stop()
		return err
	}
	return nil
}

// leadCtx derives a detached context for a fetch that must outlive its client.
func (c *Cache) leadCtx() (context.Context, context.CancelFunc) {
	if c.cfg.CollapseLeadTimeout > 0 {
		return context.WithTimeout(c.root, c.cfg.CollapseLeadTimeout)
	}
	return context.WithCancel(c.root)
}

// now reads the injected clock.
func (c *Cache) now() time.Time { return c.cfg.Now() }

// getBuf takes a copy buffer from the pool; putBuf returns it.
func (c *Cache) getBuf() *[]byte {
	if b, ok := c.bufs.Get().(*[]byte); ok {
		return b
	}
	b := make([]byte, copyBufSize)
	return &b
}

func (c *Cache) putBuf(b *[]byte) { c.bufs.Put(b) }

// tagsOf reads the surrogate keys from h.
func (c *Cache) tagsOf(h http.Header) []string {
	var out []string
	for _, name := range c.tagHeaders {
		for _, v := range h[name] {
			for t := range strings.SplitSeq(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// registerVariant records key under k0, counting an eviction when the store's
// LRU cap will drop one to make room.
func (c *Cache) registerVariant(ctx context.Context, k0, key cachekey.Key, vary []string, existing []store.Variant, now time.Time) {
	present := false
	for _, v := range existing {
		if v.Key == key {
			present = true
			break
		}
	}
	if !present && c.cfg.MaxVariants > 0 && len(existing) >= c.cfg.MaxVariants {
		c.rec.VariantEvicted()
		if _, seen := c.warned.LoadOrStore(k0, struct{}{}); !seen {
			c.log.Warn("cache: variant cap reached", slog.String("primary", k0.String()), slog.Int("max", c.cfg.MaxVariants))
		}
	}
	if err := c.store.PutVariant(ctx, k0, store.Variant{Vary: vary, Key: key, Seen: now}); err != nil {
		c.log.Warn("cache: put variant", slog.String("primary", k0.String()), slog.Any("err", err))
	}
}

// keyFor is the entry key for a request: the primary key when the response
// does not vary, else the Vary-selected secondary key.
func keyFor(k0 cachekey.Key, vary []string, h http.Header) cachekey.Key {
	if len(vary) == 0 {
		return k0
	}
	return cachekey.Secondary(k0, vary, h)
}

// canonicalNames canonicalises header names, dropping blanks.
func canonicalNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, textproto.CanonicalMIMEHeaderKey(n))
		}
	}
	return out
}

// canonicalPrefix canonicalises an absolute URL prefix so it matches the
// canonical URLs the tag index records; anything else is used verbatim.
func canonicalPrefix(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return p
	}
	if u.RawQuery == "" && !strings.HasSuffix(p, "?") {
		return cachekey.Canonical(u)
	}
	return p
}
