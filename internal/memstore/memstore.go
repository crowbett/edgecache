// Package memstore is the memory tier: a sharded LRU with a byte budget whose
// bodies are immutable []byte shared with readers (Open is a bytes.Reader,
// eviction is a map delete). Zero HTTP semantics.
//
// STUB written by the integrator to freeze the contract; workstream B owns this
// package and replaces the bodies, keeping every exported signature.
package memstore

import (
	"context"
	"errors"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// Config sizes the memory tier.
type Config struct {
	// Capacity is the byte budget across all shards (EDGECACHE_MEM_BYTES).
	Capacity int64
	// Shards is the shard count, a power of two in [1,256] (EDGECACHE_MEM_SHARDS).
	Shards int
	// MaxObjectBytes is the admission cap (EDGECACHE_MEM_MAX_OBJECT_BYTES).
	MaxObjectBytes int64
	// MaxVariants caps the Variant list per primary key (EDGECACHE_MAX_VARIANTS).
	MaxVariants int
}

// Store is the memory tier. It satisfies cache.Store.
type Store struct {
	cfg Config
}

// New builds a memory tier; it validates cfg (positive capacity, power-of-two
// shards) and never touches the filesystem.
func New(cfg Config) (*Store, error) {
	return &Store{cfg: cfg}, nil
}

// Get returns the held entry under k or store.ErrMiss.
func (s *Store) Get(ctx context.Context, k cachekey.Key) (*store.Entry, error) {
	_, _, _ = s, ctx, k
	return nil, store.ErrMiss
}

// Writer opens a body writer for k; Commit publishes it atomically, evicting LRU
// tails until it fits, and returns store.ErrTooLarge past MaxObjectBytes.
func (s *Store) Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	_, _, _, _ = s, ctx, k, m
	return nil, errors.New("stub: memstore.Store.Writer")
}

// UpdateMeta replaces the metadata of an existing entry (304 merge); the body is untouched.
func (s *Store) UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error {
	_, _, _, _ = s, ctx, k, m
	return errors.New("stub: memstore.Store.UpdateMeta")
}

// Delete removes the given keys and reports how many existed.
func (s *Store) Delete(ctx context.Context, keys ...cachekey.Key) (int, error) {
	_, _, _ = s, ctx, keys
	return 0, errors.New("stub: memstore.Store.Delete")
}

// Variants lists the Vary variants recorded under primary, most recent first.
func (s *Store) Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error) {
	_, _, _ = s, ctx, primary
	return nil, errors.New("stub: memstore.Store.Variants")
}

// PutVariant records v under primary, evicting the LRU variant (and its entry) past MaxVariants.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	_, _, _, _ = s, ctx, primary, v
	return errors.New("stub: memstore.Store.PutVariant")
}

// Stats returns the tier's gauges keyed by store.TierMem.
func (s *Store) Stats() map[string]store.Stats {
	return map[string]store.Stats{store.TierMem: {Capacity: s.cfg.Capacity}}
}

// Close drops every entry; further calls return store.ErrClosed.
func (s *Store) Close() error {
	_ = s
	return nil
}
