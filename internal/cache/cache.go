// Package cache is the engine: it joins the key derivation, the RFC 9111
// policy, the store and the origin into the catch-all proxy handler (hit, miss,
// revalidate, stale and bypass paths, the tee copy, orphan fetch) plus Purge.
// Its ports (Store, Fetcher, Recorder) are defined here, where they are
// consumed; memstore/diskstore/tiered, *origin.Origin and *metrics.Registry
// satisfy them.
//
// STUB written by the integrator to freeze the contract; workstream C owns this
// package and replaces the bodies, keeping every exported signature. Store,
// Fetcher and Recorder are frozen at step 1 and change only through the
// integrator.
package cache

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
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
}

// New wires the engine; it rebuilds the tag index when s implements Walker.
func New(cfg Config, s Store, f Fetcher, rec Recorder, log *slog.Logger) (*Cache, error) {
	if s == nil || f == nil || rec == nil {
		return nil, errors.New("cache: store, fetcher and recorder are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Cache{cfg: cfg, store: s, fetch: f, rec: rec, log: log}, nil
}

// Handler returns the catch-all proxy handler. It is mounted without
// http.TimeoutHandler; its budget comes from the origin context.
func (c *Cache) Handler() http.Handler {
	_ = c
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Cache", "BYPASS")
		w.Header().Set("X-Cache-Reason", string(rfc9111.ReasonNotImplemented))
		http.Error(w, "stub: cache.Cache.Handler", http.StatusNotImplemented)
	})
}

// Purge deletes by URL (every variant), prefix or tag and records
// edgecache_purges_total{kind}.
func (c *Cache) Purge(ctx context.Context, req PurgeRequest) (PurgeResult, error) {
	_, _, _ = c, ctx, req
	return PurgeResult{}, errors.New("stub: cache.Cache.Purge")
}

// Run is the janitor: every JanitorInterval it sweeps the store (Sweeper) and
// pushes StoreGauges. It returns when ctx is done.
func (c *Cache) Run(ctx context.Context) {
	_ = c
	<-ctx.Done()
}

// Drain waits for in-flight background revalidations to finish or ctx to end.
func (c *Cache) Drain(ctx context.Context) error {
	_, _ = c, ctx
	return nil
}
