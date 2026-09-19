// Package tiered puts the memory tier in front of the disk tier: Get tries
// memory then disk and promotes a disk hit that fits; Writer fans out into a
// memory buffer and the disk writer, dropping the memory half past the memory
// admission cap. A nil disk tier gives a pure-memory cache.
//
// STUB written by the integrator to freeze the contract; workstream B owns this
// package and replaces the bodies, keeping every exported signature.
package tiered

import (
	"context"
	"errors"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// Tier is what tiered needs from each layer; *memstore.Store and
// *diskstore.Store satisfy it (it mirrors cache.Store so tests can fake a tier).
type Tier interface {
	Get(ctx context.Context, k cachekey.Key) (*store.Entry, error)
	Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error)
	UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error
	Delete(ctx context.Context, keys ...cachekey.Key) (int, error)
	Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error)
	PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error
	Stats() map[string]store.Stats
	Close() error
}

// Config tunes promotion and fan-out.
type Config struct {
	// MemMaxObjectBytes is the largest body admitted to (or promoted into) memory.
	MemMaxObjectBytes int64
}

// Store is the two-tier composite. It satisfies cache.Store and cache.Walker.
type Store struct {
	cfg  Config
	mem  Tier
	disk Tier // nil = memory only
}

// New composes mem in front of disk; disk may be nil.
func New(cfg Config, mem, disk Tier) *Store {
	return &Store{cfg: cfg, mem: mem, disk: disk}
}

// Get tries memory then disk, promoting a disk hit that fits MemMaxObjectBytes.
func (s *Store) Get(ctx context.Context, k cachekey.Key) (*store.Entry, error) {
	_, _, _ = s, ctx, k
	return nil, store.ErrMiss
}

// Writer fans out into both tiers; the memory half is dropped past MemMaxObjectBytes.
func (s *Store) Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	_, _, _, _ = s, ctx, k, m
	return nil, errors.New("stub: tiered.Store.Writer")
}

// UpdateMeta updates every tier holding k.
func (s *Store) UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error {
	_, _, _, _ = s, ctx, k, m
	return errors.New("stub: tiered.Store.UpdateMeta")
}

// Delete removes keys from every tier and reports the count of distinct keys that existed.
func (s *Store) Delete(ctx context.Context, keys ...cachekey.Key) (int, error) {
	_, _, _ = s, ctx, keys
	return 0, errors.New("stub: tiered.Store.Delete")
}

// Variants merges the variant lists of both tiers, most recent first.
func (s *Store) Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error) {
	_, _, _ = s, ctx, primary
	return nil, errors.New("stub: tiered.Store.Variants")
}

// PutVariant records v in every tier.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	_, _, _, _ = s, ctx, primary, v
	return errors.New("stub: tiered.Store.PutVariant")
}

// Stats merges the per-tier gauges ("mem", "disk").
func (s *Store) Stats() map[string]store.Stats {
	out := map[string]store.Stats{}
	for _, t := range []Tier{s.mem, s.disk} {
		if t == nil {
			continue
		}
		for k, v := range t.Stats() {
			out[k] = v
		}
	}
	return out
}

// Walk enumerates the disk tier's metas (memory entries are a subset after
// promotion); with no disk tier it walks memory.
func (s *Store) Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error {
	_, _, _ = s, ctx, fn
	return errors.New("stub: tiered.Store.Walk")
}

// Sweep forwards the janitor tick to the disk tier; a no-op without one.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	_, _ = s, ctx
	return 0, errors.New("stub: tiered.Store.Sweep")
}

// Close closes memory then disk, returning the first error.
func (s *Store) Close() error {
	var errs []error
	for _, t := range []Tier{s.mem, s.disk} {
		if t != nil {
			errs = append(errs, t.Close())
		}
	}
	return errors.Join(errs...)
}
