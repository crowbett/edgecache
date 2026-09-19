// Package tiered puts the memory tier in front of the disk tier: Get tries
// memory then disk and promotes a disk hit that fits; Writer fans out into a
// memory buffer and the disk writer, dropping the memory half past the memory
// admission cap. A nil disk tier gives a pure-memory cache (and a nil memory
// tier a pure-disk one). Zero HTTP semantics.
//
// Commit semantics: the disk tier is authoritative. A disk commit failure
// aborts the memory half and fails the commit; a memory commit failure (its
// budget pinned by held entries) keeps the disk entry and returns it. The
// returned entry is the memory one when both committed.
package tiered

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// ErrCommitted is returned by a Writer used after Commit or Abort.
var ErrCommitted = errors.New("tiered: writer already committed")

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

// walker and sweeper are the optional per-tier hooks Walk and Sweep delegate to.
type walker interface {
	Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error
}

type sweeper interface {
	Sweep(ctx context.Context) (int, error)
}

// Config tunes promotion and fan-out.
type Config struct {
	// MemMaxObjectBytes is the largest body admitted to (or promoted into)
	// memory; zero leaves it to the memory tier's own admission cap.
	MemMaxObjectBytes int64
}

// Counters are the composite's monotonic event counts.
type Counters struct {
	// Promotions counts disk hits copied into memory; PromotionFailures counts
	// disk hits served from disk because the copy or memory commit failed.
	Promotions, PromotionFailures int64
}

// Store is the two-tier composite. It satisfies cache.Store, cache.Walker and cache.Sweeper.
type Store struct {
	cfg  Config
	mem  Tier // nil = disk only
	disk Tier // nil = memory only

	promotions, promotionFailures atomic.Int64
}

// New composes mem in front of disk; disk may be nil.
func New(cfg Config, mem, disk Tier) *Store {
	return &Store{cfg: cfg, mem: mem, disk: disk}
}

// Counters returns the promotion counts.
func (s *Store) Counters() Counters {
	return Counters{Promotions: s.promotions.Load(), PromotionFailures: s.promotionFailures.Load()}
}

func (s *Store) fitsMem(size int64) bool {
	return s.mem != nil && (s.cfg.MemMaxObjectBytes <= 0 || size <= s.cfg.MemMaxObjectBytes)
}

// Get tries memory then disk, promoting a disk hit that fits MemMaxObjectBytes.
// A promoted hit is returned as the memory entry; otherwise the disk entry.
func (s *Store) Get(ctx context.Context, k cachekey.Key) (*store.Entry, error) {
	if s.mem != nil {
		e, err := s.mem.Get(ctx, k)
		if err == nil {
			return e, nil
		}
		if !errors.Is(err, store.ErrMiss) {
			return nil, err
		}
	}
	if s.disk == nil {
		return nil, store.ErrMiss
	}
	de, err := s.disk.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if !s.fitsMem(de.Meta.Size) {
		return de, nil
	}
	if me, ok := s.promote(ctx, de); ok {
		s.promotions.Add(1)
		de.Release()
		return me, nil
	}
	s.promotionFailures.Add(1)
	return de, nil // served from disk this time
}

// promote copies de's body into the memory tier and returns the held memory
// entry; ok is false when the copy or the memory commit failed (the caller then
// serves the disk entry, so the failure is not an error to it).
func (s *Store) promote(ctx context.Context, de *store.Entry) (*store.Entry, bool) {
	rc, err := de.Open()
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	w, err := s.mem.Writer(ctx, de.Key, de.Meta)
	if err != nil {
		return nil, false
	}
	defer func() { _ = w.Abort() }()
	if _, err := io.Copy(w, rc); err != nil {
		return nil, false
	}
	me, err := w.Commit(de.Meta)
	if err != nil {
		return nil, false
	}
	return me, true
}

// Writer fans out into both tiers; the memory half is dropped past MemMaxObjectBytes
// (declared up front, or as the running size crosses it).
func (s *Store) Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	var mw, dw store.Writer
	if s.disk != nil {
		w, err := s.disk.Writer(ctx, k, m)
		if err != nil {
			return nil, err
		}
		dw = w
	}
	if s.fitsMem(m.Size) {
		w, err := s.mem.Writer(ctx, k, m)
		switch {
		case err == nil:
			mw = w
		case errors.Is(err, store.ErrTooLarge) && dw != nil:
			// disk only
		default:
			if dw != nil {
				_ = dw.Abort()
			}
			return nil, err
		}
	}
	if mw == nil && dw == nil {
		return nil, fmt.Errorf("tiered: no tier admits %d bytes: %w", m.Size, store.ErrTooLarge)
	}
	return &writer{s: s, mem: mw, disk: dw}, nil
}

// UpdateMeta updates every tier holding k; store.ErrMiss only when neither does.
func (s *Store) UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error {
	found := false
	var errs []error
	for _, t := range s.tiers() {
		switch err := t.UpdateMeta(ctx, k, m); {
		case err == nil:
			found = true
		case errors.Is(err, store.ErrMiss):
		default:
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if !found {
		return store.ErrMiss
	}
	return nil
}

// Delete removes keys from every tier and reports the count of distinct keys that existed.
func (s *Store) Delete(ctx context.Context, keys ...cachekey.Key) (int, error) {
	deleted := 0
	for _, k := range keys {
		existed := false
		for _, t := range s.tiers() {
			n, err := t.Delete(ctx, k)
			if err != nil {
				return deleted, err
			}
			existed = existed || n > 0
		}
		if existed {
			deleted++
		}
	}
	return deleted, nil
}

// Variants merges the variant lists of both tiers, most recent first, one
// entry per key.
func (s *Store) Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error) {
	var (
		out  []store.Variant
		errs []error
	)
	seen := make(map[cachekey.Key]int)
	for _, t := range s.tiers() {
		vs, err := t.Variants(ctx, primary)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, v := range vs {
			if i, ok := seen[v.Key]; ok {
				if v.Seen.After(out[i].Seen) {
					out[i] = v
				}
				continue
			}
			seen[v.Key] = len(out)
			out = append(out, v)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seen.After(out[j].Seen) })
	return out, nil
}

// PutVariant records v in every tier.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	var errs []error
	for _, t := range s.tiers() {
		if err := t.PutVariant(ctx, primary, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Stats merges the per-tier gauges ("mem", "disk").
func (s *Store) Stats() map[string]store.Stats {
	out := map[string]store.Stats{}
	for _, t := range s.tiers() {
		for k, v := range t.Stats() {
			out[k] = v
		}
	}
	return out
}

// Walk enumerates the disk tier's metas (memory entries are a subset after
// promotion); with no disk tier it walks memory. A tier without Walk yields nothing.
func (s *Store) Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error {
	t := s.disk
	if t == nil {
		t = s.mem
	}
	if w, ok := t.(walker); ok {
		return w.Walk(ctx, fn)
	}
	return nil
}

// Sweep forwards the janitor tick to the disk tier; a no-op without one.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	if sw, ok := s.disk.(sweeper); ok {
		return sw.Sweep(ctx)
	}
	return 0, nil
}

// Close closes memory then disk, returning the joined errors.
func (s *Store) Close() error {
	tiers := s.tiers()
	errs := make([]error, 0, len(tiers))
	for _, t := range tiers {
		errs = append(errs, t.Close())
	}
	return errors.Join(errs...)
}

// tiers lists the configured tiers, memory first.
func (s *Store) tiers() []Tier {
	ts := make([]Tier, 0, 2)
	if s.mem != nil {
		ts = append(ts, s.mem)
	}
	if s.disk != nil {
		ts = append(ts, s.disk)
	}
	return ts
}

// writer fans one body out to the memory and disk writers.
type writer struct {
	s    *Store
	mem  store.Writer // nil once dropped
	disk store.Writer // nil in memory-only mode
	n    int64
	err  error // sticky: a memory-only body that crossed the cap
	done bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrCommitted
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.disk != nil {
		if n, err := w.disk.Write(p); err != nil {
			return n, err
		}
	}
	if w.mem != nil {
		w.n += int64(len(p))
		var err error
		if !w.s.fitsMem(w.n) {
			err = fmt.Errorf("tiered: body over the %d byte memory cap: %w", w.s.cfg.MemMaxObjectBytes, store.ErrTooLarge)
		} else {
			_, err = w.mem.Write(p)
		}
		if err != nil {
			w.dropMem()
			if w.disk == nil {
				w.err = err
				return 0, err
			}
			// Past the memory cap or budget: disk keeps the body.
		}
	}
	return len(p), nil
}

func (w *writer) dropMem() {
	_ = w.mem.Abort()
	w.mem = nil
}

// Commit publishes in every tier that kept the body; see the package doc for
// which entry comes back.
func (w *writer) Commit(final store.Meta) (*store.Entry, error) {
	if w.done {
		return nil, ErrCommitted
	}
	w.done = true
	if w.err != nil {
		return nil, w.err
	}
	if w.mem == nil && w.disk == nil {
		return nil, fmt.Errorf("tiered: no tier kept the body: %w", store.ErrTooLarge)
	}
	var de *store.Entry
	if w.disk != nil {
		e, err := w.disk.Commit(final)
		if err != nil {
			if w.mem != nil {
				w.dropMem()
			}
			return nil, err
		}
		de = e
	}
	if w.mem != nil {
		me, err := w.mem.Commit(final)
		if err == nil {
			if de != nil {
				de.Release()
			}
			return me, nil
		}
		if de == nil {
			return nil, err
		}
	}
	return de, nil
}

// Abort discards both halves; it is idempotent and a no-op after Commit.
func (w *writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	var errs []error
	if w.mem != nil {
		errs = append(errs, w.mem.Abort())
		w.mem = nil
	}
	if w.disk != nil {
		errs = append(errs, w.disk.Abort())
	}
	return errors.Join(errs...)
}
