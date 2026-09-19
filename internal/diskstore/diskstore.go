// Package diskstore is the disk tier: content-addressed files
// (<dir>/<aa>/<bb>/<hex>.body + .meta JSON) written temp+Sync+rename with meta
// last, an in-memory LRU index rebuilt by scanning at start, refcounts and a
// tombstone list for entries evicted while a reader holds them (NTFS will not
// unlink an open file). Zero HTTP semantics.
//
// STUB written by the integrator to freeze the contract; workstream B owns this
// package and replaces the bodies, keeping every exported signature.
package diskstore

import (
	"context"
	"errors"
	"log/slog"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// Config sizes the disk tier.
type Config struct {
	// Dir is the cache root (EDGECACHE_DISK_DIR); it is created if missing.
	Dir string
	// Capacity is the byte budget, tombstones counted as resident (EDGECACHE_DISK_BYTES).
	Capacity int64
	// MaxObjectBytes is the admission cap (EDGECACHE_MAX_OBJECT_BYTES).
	MaxObjectBytes int64
	// MaxVariants caps the Variant list per primary key (EDGECACHE_MAX_VARIANTS).
	MaxVariants int
}

// Store is the disk tier. It satisfies cache.Store and cache.Walker.
type Store struct {
	cfg Config
	log *slog.Logger
}

// New opens (or creates) the directory tree, sweeps partial writes and rebuilds
// the index from the .meta files; a corrupt meta deletes its pair and logs,
// never failing startup.
func New(cfg Config, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	return &Store{cfg: cfg, log: log}, nil
}

// Get returns the held entry under k (refcount +1) or store.ErrMiss.
func (s *Store) Get(ctx context.Context, k cachekey.Key) (*store.Entry, error) {
	_, _, _ = s, ctx, k
	return nil, store.ErrMiss
}

// Writer streams a body to <dir>/tmp/<hex>.<rand>; Commit fsyncs, renames the
// body then the meta, and evicts LRU tails until the budget fits.
func (s *Store) Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	_, _, _, _ = s, ctx, k, m
	return nil, errors.New("stub: diskstore.Store.Writer")
}

// UpdateMeta rewrites the .meta file atomically (304 merge); the body is untouched.
func (s *Store) UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error {
	_, _, _, _ = s, ctx, k, m
	return errors.New("stub: diskstore.Store.UpdateMeta")
}

// Delete unlinks the given keys (tombstoning any that are held) and reports how many existed.
func (s *Store) Delete(ctx context.Context, keys ...cachekey.Key) (int, error) {
	_, _, _ = s, ctx, keys
	return 0, errors.New("stub: diskstore.Store.Delete")
}

// Variants lists the Vary variants recorded under primary, most recent first.
func (s *Store) Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error) {
	_, _, _ = s, ctx, primary
	return nil, errors.New("stub: diskstore.Store.Variants")
}

// PutVariant records v under primary, evicting the LRU variant (and its entry) past MaxVariants.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	_, _, _, _ = s, ctx, primary, v
	return errors.New("stub: diskstore.Store.PutVariant")
}

// Stats returns the tier's gauges keyed by store.TierDisk.
func (s *Store) Stats() map[string]store.Stats {
	return map[string]store.Stats{store.TierDisk: {Capacity: s.cfg.Capacity}}
}

// Walk calls fn for every indexed entry's key and meta (used to rebuild the tag
// index after scan recovery); a non-nil error from fn stops the walk.
func (s *Store) Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error {
	_, _, _ = s, ctx, fn
	return errors.New("stub: diskstore.Store.Walk")
}

// Sweep is the janitor hook: it retries tombstoned unlinks and removes stale
// temp files, returning how many files it freed. The engine calls it every
// EDGECACHE_JANITOR_INTERVAL.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	_, _ = s, ctx
	return 0, errors.New("stub: diskstore.Store.Sweep")
}

// Close flushes metas and releases tombstones; further calls return store.ErrClosed.
func (s *Store) Close() error {
	_ = s
	return nil
}
