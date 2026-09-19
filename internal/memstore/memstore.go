// Package memstore is the memory tier: a sharded LRU with a byte budget whose
// bodies are immutable []byte shared with readers (Open is a bytes.Reader,
// eviction is a map delete). Zero HTTP semantics.
//
// Budget: every shard owns Capacity/Shards bytes and evicts its own unheld LRU
// tail to stay inside that share; a global counter allows a small overdraft
// (Capacity/16) so a shard blocked by held entries, or an object larger than
// one share, can still be admitted after the other shards have been trimmed.
// Entries are refcounted: Get holds, Release drops, and a held entry is never
// evicted; Delete (or an overwrite) detaches a held entry from the index and
// its bytes stay charged until the last reader releases it.
package memstore

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// Defaults applied by New when a Config field is zero.
const (
	DefaultShards      = 16
	DefaultMaxVariants = 8
	maxShards          = 256
)

// ErrCommitted is returned by a Writer used after Commit.
var ErrCommitted = errors.New("memstore: writer already committed")

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

// Counters are the tier's monotonic event counts (Stats carries the gauges).
type Counters struct {
	Hits, Misses, Evictions, VariantEvictions int64
}

// node is one stored object. body is immutable once published; readers keep it
// alive through the GC, the refcount only governs byte accounting and eviction.
// refs and tick are atomic so Get runs under the shard's read lock; everything
// else is guarded by the shard's write lock.
type node struct {
	key  cachekey.Key
	meta store.Meta
	body []byte
	size int64
	refs atomic.Int32
	tick atomic.Int64 // shard clock at the last Get

	placed   int64 // shard clock when last placed at the LRU front
	detached bool  // out of the index; bytes stay charged until refs hits zero
	freed    bool
	elem     *list.Element
	sh       *shard

	// Bound once so Get does not allocate a closure per call.
	openFn    func() (io.ReadCloser, error)
	releaseFn func()
}

type shard struct {
	mu    sync.RWMutex
	s     *Store
	m     map[cachekey.Key]*node
	lru   *list.List // front = most recently placed; Get stamps ticks, eviction reorders
	bytes int64      // resident + detached-but-held
	vars  map[cachekey.Key][]store.Variant

	clock        atomic.Int64
	hits, misses atomic.Int64
}

// Store is the memory tier. It satisfies cache.Store.
type Store struct {
	cfg       Config
	shards    []*shard
	share     int64 // per-shard byte share
	overdraft int64 // global slack past Capacity
	total     atomic.Int64
	closed    atomic.Bool

	evictions, varEvictions atomic.Int64
}

// New builds a memory tier; it validates cfg (positive capacity, power-of-two
// shards) and never touches the filesystem. Zero Shards, MaxObjectBytes and
// MaxVariants take the defaults (16, Capacity, 8).
func New(cfg Config) (*Store, error) {
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("memstore: capacity must be positive, got %d", cfg.Capacity)
	}
	if cfg.Shards == 0 {
		cfg.Shards = DefaultShards
	}
	if cfg.Shards < 1 || cfg.Shards > maxShards || cfg.Shards&(cfg.Shards-1) != 0 {
		return nil, fmt.Errorf("memstore: shards must be a power of two in [1,%d], got %d", maxShards, cfg.Shards)
	}
	if cfg.MaxObjectBytes <= 0 || cfg.MaxObjectBytes > cfg.Capacity {
		cfg.MaxObjectBytes = cfg.Capacity
	}
	if cfg.MaxVariants <= 0 {
		cfg.MaxVariants = DefaultMaxVariants
	}
	s := &Store{
		cfg:       cfg,
		shards:    make([]*shard, cfg.Shards),
		share:     cfg.Capacity / int64(cfg.Shards),
		overdraft: max(cfg.Capacity/16, 1),
	}
	for i := range s.shards {
		s.shards[i] = &shard{s: s, m: make(map[cachekey.Key]*node), lru: list.New(), vars: make(map[cachekey.Key][]store.Variant)}
	}
	return s, nil
}

// Overdraft is the number of bytes Stats().Bytes may exceed Capacity by while
// held entries block eviction; the tier never goes past Capacity+Overdraft.
func (s *Store) Overdraft() int64 { return s.overdraft }

// Counters returns the hit/miss/eviction counts.
func (s *Store) Counters() Counters {
	c := Counters{Evictions: s.evictions.Load(), VariantEvictions: s.varEvictions.Load()}
	for _, sh := range s.shards {
		c.Hits += sh.hits.Load()
		c.Misses += sh.misses.Load()
	}
	return c
}

func (s *Store) shard(k cachekey.Key) *shard { return s.shards[int(k[0])&(len(s.shards)-1)] }

// Get returns the held entry under k or store.ErrMiss.
func (s *Store) Get(_ context.Context, k cachekey.Key) (*store.Entry, error) {
	if s.closed.Load() {
		return nil, store.ErrClosed
	}
	sh := s.shard(k)
	sh.mu.RLock()
	n := sh.m[k]
	if n == nil {
		sh.mu.RUnlock()
		sh.misses.Add(1)
		return nil, store.ErrMiss
	}
	n.refs.Add(1)
	n.tick.Store(sh.clock.Add(1))
	e := store.NewEntry(k, n.meta, store.TierMem, n.openFn, n.releaseFn)
	sh.mu.RUnlock()
	sh.hits.Add(1)
	return e, nil
}

// Writer opens a body writer for k; Commit publishes it atomically, evicting LRU
// tails until it fits, and returns store.ErrTooLarge past MaxObjectBytes.
func (s *Store) Writer(_ context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	if s.closed.Load() {
		return nil, store.ErrClosed
	}
	if m.Size > s.cfg.MaxObjectBytes {
		return nil, fmt.Errorf("memstore: %d bytes over the %d cap: %w", m.Size, s.cfg.MaxObjectBytes, store.ErrTooLarge)
	}
	w := &writer{s: s, key: k}
	if m.Size > 0 {
		w.buf = make([]byte, 0, m.Size)
	}
	return w, nil
}

// UpdateMeta replaces the metadata of an existing entry (304 merge); the body is untouched.
func (s *Store) UpdateMeta(_ context.Context, k cachekey.Key, m store.Meta) error {
	if s.closed.Load() {
		return store.ErrClosed
	}
	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	n := sh.m[k]
	if n == nil {
		return store.ErrMiss
	}
	m.Size = n.size
	n.meta = m
	return nil
}

// Delete removes the given keys and reports how many existed. A held entry is
// detached (invisible to Get) and its bytes are released with its last reader.
func (s *Store) Delete(_ context.Context, keys ...cachekey.Key) (int, error) {
	if s.closed.Load() {
		return 0, store.ErrClosed
	}
	deleted := 0
	for _, k := range keys {
		sh := s.shard(k)
		sh.mu.Lock()
		if n := sh.m[k]; n != nil {
			sh.detach(n)
			deleted++
		}
		delete(sh.vars, k)
		sh.mu.Unlock()
	}
	return deleted, nil
}

// Variants lists the Vary variants recorded under primary, most recent first.
func (s *Store) Variants(_ context.Context, primary cachekey.Key) ([]store.Variant, error) {
	if s.closed.Load() {
		return nil, store.ErrClosed
	}
	sh := s.shard(primary)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	vs := sh.vars[primary]
	if len(vs) == 0 {
		return nil, nil
	}
	out := make([]store.Variant, len(vs))
	copy(out, vs)
	return out, nil
}

// PutVariant records v under primary, evicting the LRU variant (and its entry) past MaxVariants.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	if s.closed.Load() {
		return store.ErrClosed
	}
	sh := s.shard(primary)
	sh.mu.Lock()
	vs := sh.vars[primary]
	for i := range vs {
		if vs[i].Key == v.Key {
			vs = append(vs[:i], vs[i+1:]...)
			break
		}
	}
	vs = append(vs, store.Variant{})
	copy(vs[1:], vs)
	vs[0] = v
	var evicted *store.Variant
	if len(vs) > s.cfg.MaxVariants {
		old := vs[len(vs)-1]
		evicted = &old
		vs = vs[:len(vs)-1]
	}
	sh.vars[primary] = vs
	sh.mu.Unlock()
	if evicted != nil {
		s.varEvictions.Add(1)
		if n, _ := s.Delete(ctx, evicted.Key); n > 0 {
			s.evictions.Add(int64(n))
		}
	}
	return nil
}

// Stats returns the tier's gauges keyed by store.TierMem. Bytes includes
// detached entries still held by readers.
func (s *Store) Stats() map[string]store.Stats {
	var entries int64
	for _, sh := range s.shards {
		sh.mu.RLock()
		entries += int64(len(sh.m))
		sh.mu.RUnlock()
	}
	return map[string]store.Stats{store.TierMem: {
		Bytes:     s.total.Load(),
		Entries:   entries,
		Evictions: s.evictions.Load(),
		Capacity:  s.cfg.Capacity,
	}}
}

// Walk calls fn for every resident entry's key and meta; a non-nil error from
// fn stops the walk. It snapshots each shard, so fn may call back into the store.
func (s *Store) Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error {
	if s.closed.Load() {
		return store.ErrClosed
	}
	for _, sh := range s.shards {
		sh.mu.RLock()
		snap := make([]*node, 0, len(sh.m))
		for _, n := range sh.m {
			snap = append(snap, n)
		}
		sh.mu.RUnlock()
		for _, n := range snap {
			if err := ctx.Err(); err != nil {
				return err
			}
			sh.mu.RLock()
			m := n.meta
			sh.mu.RUnlock()
			if err := fn(n.key, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close drops every entry; further calls return store.ErrClosed. Bytes of
// entries still held by readers are released as they are.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return store.ErrClosed
	}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, n := range sh.m {
			sh.detach(n)
		}
		sh.vars = make(map[cachekey.Key][]store.Variant)
		sh.mu.Unlock()
	}
	return nil
}

// publish inserts body under k, evicting as needed; it returns a held entry.
func (s *Store) publish(k cachekey.Key, m store.Meta, body []byte) (*store.Entry, error) {
	if s.closed.Load() {
		return nil, store.ErrClosed
	}
	size := int64(len(body))
	m.Size = size
	sh := s.shard(k)
	for attempt := 0; attempt < 2; attempt++ {
		sh.mu.Lock()
		if old := sh.m[k]; old != nil {
			sh.detach(old)
		}
		sh.evictUntil(s.share - size)
		if s.reserve(size) {
			n := &node{key: k, meta: m, body: body, size: size, sh: sh}
			n.refs.Store(1)
			n.placed = sh.clock.Add(1)
			n.tick.Store(n.placed)
			n.openFn = n.open
			n.releaseFn = n.release
			n.elem = sh.lru.PushFront(n)
			sh.m[k] = n
			sh.bytes += size
			sh.mu.Unlock()
			return store.NewEntry(k, m, store.TierMem, n.openFn, n.releaseFn), nil
		}
		sh.mu.Unlock()
		if attempt == 0 {
			s.reclaim(size)
		}
	}
	return nil, fmt.Errorf("memstore: no room for %d bytes (held entries pin the budget): %w", size, store.ErrTooLarge)
}

// reserve charges size to the global counter unless it would pass Capacity+overdraft.
func (s *Store) reserve(size int64) bool {
	limit := s.cfg.Capacity + s.overdraft
	for {
		cur := s.total.Load()
		if cur+size > limit {
			return false
		}
		if s.total.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

// reclaim trims every shard's unheld tail until size fits under
// Capacity+overdraft; it takes one shard lock at a time, so it never deadlocks
// with a concurrent publish.
func (s *Store) reclaim(size int64) {
	limit := s.cfg.Capacity + s.overdraft
	for _, sh := range s.shards {
		need := s.total.Load() + size - limit
		if need <= 0 {
			return
		}
		sh.mu.Lock()
		sh.evictUntil(sh.bytes - need)
		sh.mu.Unlock()
	}
}

// evictUntil (locked) removes unheld LRU tails until sh.bytes <= limit or no
// unheld entry remains. A tail node read since it was last placed gets a second
// chance (moved to the front, CLOCK style), so Get never takes the write lock;
// held entries are skipped, not detached.
func (sh *shard) evictUntil(limit int64) {
	var stop *list.Element // first element re-placed this pass; the walk ends there
	for e := sh.lru.Back(); e != nil && e != stop && sh.bytes > limit; {
		prev := e.Prev()
		n, ok := e.Value.(*node)
		if !ok {
			e = prev
			continue
		}
		if t := n.tick.Load(); t > n.placed {
			n.placed = t
			sh.lru.MoveToFront(e)
			if stop == nil {
				stop = e
			}
		} else if n.refs.Load() == 0 {
			sh.detach(n)
			sh.s.evictions.Add(1)
		}
		e = prev
	}
}

// detach (locked) removes n from the index; its bytes are freed now if unheld,
// otherwise when the last reader releases it.
func (sh *shard) detach(n *node) {
	if n.detached {
		return
	}
	n.detached = true
	delete(sh.m, n.key)
	sh.lru.Remove(n.elem)
	if n.refs.Load() == 0 {
		sh.free(n)
	}
}

// free (locked) releases n's bytes exactly once.
func (sh *shard) free(n *node) {
	if n.freed {
		return
	}
	n.freed = true
	sh.bytes -= n.size
	sh.s.total.Add(-n.size)
}

func (n *node) open() (io.ReadCloser, error) {
	r := &bodyReader{}
	r.Reset(n.body)
	return r, nil
}

func (n *node) release() {
	if n.refs.Add(-1) != 0 {
		return
	}
	sh := n.sh
	sh.mu.Lock()
	if n.detached {
		sh.free(n)
	}
	sh.mu.Unlock()
}

// bodyReader is a bytes.Reader with a no-op Close (one alloc per Open).
type bodyReader struct{ bytes.Reader }

// Close is a no-op; the body is shared and immutable.
func (*bodyReader) Close() error { return nil }

// writer buffers the body up to MaxObjectBytes and publishes on Commit.
type writer struct {
	s    *Store
	key  cachekey.Key
	buf  []byte
	err  error
	done bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrCommitted
	}
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(w.buf))+int64(len(p)) > w.s.cfg.MaxObjectBytes {
		w.err = fmt.Errorf("memstore: body over the %d byte cap: %w", w.s.cfg.MaxObjectBytes, store.ErrTooLarge)
		w.buf = nil
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Commit publishes the buffered body under the writer's key and returns a held entry.
func (w *writer) Commit(final store.Meta) (*store.Entry, error) {
	if w.done {
		return nil, ErrCommitted
	}
	w.done = true
	if w.err != nil {
		return nil, w.err
	}
	body := w.buf
	w.buf = nil
	return w.s.publish(w.key, final, body)
}

// Abort discards the buffer; it is idempotent and a no-op after Commit.
func (w *writer) Abort() error {
	w.done = true
	w.buf = nil
	return nil
}
