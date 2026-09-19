// Package diskstore is the disk tier: content-addressed files
// (<dir>/<aa>/<hex>.body + <hex>.meta JSON, aa = the first key byte) written to
// <dir>/tmp, Synced and renamed into place with the meta LAST, an in-memory LRU
// index rebuilt by scanning at start, refcounts and a tombstone list for
// entries evicted while a reader holds them (NTFS will not unlink or replace
// an open file). Zero HTTP semantics.
//
// Recovery: a .body without a .meta is a partial write and is swept; a .meta
// that does not parse, names another key, or disagrees with the body size is
// corrupt and takes its pair with it (WARN, never a startup failure).
//
// Budget: Bytes counts live entries, tombstones (evicted but still held or
// still open) and in-flight commits. Commit evicts LRU tails (held ones become
// tombstones) until the object fits; a small overdraft (Capacity/16) lets a
// commit through while held entries pin the budget, past that it is refused
// with store.ErrTooLarge.
package diskstore

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// DefaultMaxVariants is applied by New when Config.MaxVariants is zero.
const DefaultMaxVariants = 8

const (
	metaVersion  = 1
	tmpDirName   = "tmp"
	bodyExt      = ".body"
	metaExt      = ".meta"
	writeBufSize = 64 << 10
	pubStripes   = 64
	dirPerm      = 0o750
	filePerm     = 0o600
)

var (
	// ErrCommitted is returned by a Writer used after Commit or Abort.
	ErrCommitted = errors.New("diskstore: writer already committed")
	// ErrBusy is returned by Commit when the previous body under the same key
	// is still open by a reader, so the rename over it was refused by the OS
	// (Windows sharing violation). The previous entry stays served; the new
	// response is simply not stored.
	ErrBusy = errors.New("diskstore: previous body still open")
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

// Counters are the tier's monotonic event counts (Stats carries the gauges).
// Orphans and Corrupt are recovery counts from New; Deferred counts unlinks the
// OS refused (tombstone kept for Sweep); Swept counts files Sweep freed.
type Counters struct {
	Hits, Misses, Evictions, VariantEvictions int64
	Orphans, Corrupt                          int64
	Deferred, Swept                           int64
}

type entryState uint8

const (
	stateLive     entryState = iota
	stateEvicted             // out of the index; unlink once unheld (tombstone)
	stateReplaced            // out of the index; its path now belongs to a newer generation
	stateFreed               // bytes released
)

// fileEntry is one indexed object; everything is guarded by Store.mu.
type fileEntry struct {
	key   cachekey.Key
	meta  store.Meta
	size  int64
	refs  int32
	state entryState
	elem  *list.Element
	s     *Store

	// Bound once so Get does not allocate closures per call.
	openFn    func() (io.ReadCloser, error)
	releaseFn func()
}

func (fe *fileEntry) bind() {
	fe.openFn = fe.open
	fe.releaseFn = fe.release
}

// metaFile is the on-disk JSON envelope around store.Meta.
type metaFile struct {
	Version int        `json:"v"`
	Key     string     `json:"key"`
	Meta    store.Meta `json:"meta"`
}

// Store is the disk tier. It satisfies cache.Store, cache.Walker and cache.Sweeper.
type Store struct {
	cfg       Config
	log       *slog.Logger
	overdraft int64
	tmpDir    string

	mu     sync.Mutex
	closed bool
	idx    map[cachekey.Key]*fileEntry
	lru    *list.List // front = most recently accessed
	bytes  int64      // live + tombstoned + reserved by in-flight commits
	tombs  map[cachekey.Key]*fileEntry
	vars   map[cachekey.Key][]store.Variant
	temps  map[string]struct{} // temp files owned by live writers (Sweep leaves them)

	// pub serialises publish/unlink/meta rewrite per key stripe so a tombstone
	// reap never unlinks a newer generation's files.
	pub [pubStripes]sync.Mutex
	seq atomic.Uint64

	hits, misses, evictions, varEvictions atomic.Int64
	orphans, corrupt, deferred, swept     atomic.Int64
}

// New opens (or creates) the directory tree, sweeps partial writes and rebuilds
// the index from the .meta files; a corrupt meta deletes its pair and logs,
// never failing startup. Zero MaxObjectBytes and MaxVariants take the defaults
// (Capacity, 8).
func New(cfg Config, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Dir == "" {
		return nil, errors.New("diskstore: dir is required")
	}
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("diskstore: capacity must be positive, got %d", cfg.Capacity)
	}
	if cfg.MaxObjectBytes <= 0 || cfg.MaxObjectBytes > cfg.Capacity {
		cfg.MaxObjectBytes = cfg.Capacity
	}
	if cfg.MaxVariants <= 0 {
		cfg.MaxVariants = DefaultMaxVariants
	}
	cfg.Dir = filepath.Clean(cfg.Dir)
	s := &Store{
		cfg:       cfg,
		log:       log,
		overdraft: max(cfg.Capacity/16, 1),
		tmpDir:    filepath.Join(cfg.Dir, tmpDirName),
		idx:       make(map[cachekey.Key]*fileEntry),
		lru:       list.New(),
		tombs:     make(map[cachekey.Key]*fileEntry),
		vars:      make(map[cachekey.Key][]store.Variant),
		temps:     make(map[string]struct{}),
	}
	if err := os.MkdirAll(s.tmpDir, dirPerm); err != nil {
		return nil, fmt.Errorf("diskstore: create %s: %w", s.tmpDir, err)
	}
	s.clearTemps()
	if err := s.scan(); err != nil {
		return nil, err
	}
	return s, nil
}

// Overdraft is the number of bytes Stats().Bytes may exceed Capacity by while
// held entries block eviction; the tier never goes past Capacity+Overdraft.
func (s *Store) Overdraft() int64 { return s.overdraft }

// Counters returns the monotonic event counts.
func (s *Store) Counters() Counters {
	return Counters{
		Hits:             s.hits.Load(),
		Misses:           s.misses.Load(),
		Evictions:        s.evictions.Load(),
		VariantEvictions: s.varEvictions.Load(),
		Orphans:          s.orphans.Load(),
		Corrupt:          s.corrupt.Load(),
		Deferred:         s.deferred.Load(),
		Swept:            s.swept.Load(),
	}
}

// Tombstones is the number of evicted entries whose files are still on disk
// (held by a reader, or the OS refused the unlink); Sweep retries them.
func (s *Store) Tombstones() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tombs)
}

// Get returns the held entry under k (refcount +1) or store.ErrMiss.
func (s *Store) Get(_ context.Context, k cachekey.Key) (*store.Entry, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, store.ErrClosed
	}
	fe := s.idx[k]
	if fe == nil {
		s.mu.Unlock()
		s.misses.Add(1)
		return nil, store.ErrMiss
	}
	fe.refs++
	s.lru.MoveToFront(fe.elem)
	e := store.NewEntry(k, fe.meta, store.TierDisk, fe.openFn, fe.releaseFn)
	s.mu.Unlock()
	s.hits.Add(1)
	return e, nil
}

// Writer streams a body to <dir>/tmp/<hex>.<seq>.body; Commit fsyncs, renames the
// body then the meta, and evicts LRU tails until the budget fits. It returns
// store.ErrTooLarge when m.Size is past MaxObjectBytes.
func (s *Store) Writer(_ context.Context, k cachekey.Key, m store.Meta) (store.Writer, error) {
	if m.Size > s.cfg.MaxObjectBytes {
		return nil, fmt.Errorf("diskstore: %d bytes over the %d cap: %w", m.Size, s.cfg.MaxObjectBytes, store.ErrTooLarge)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, store.ErrClosed
	}
	s.mu.Unlock()
	f, path, err := s.createTemp(k, bodyExt)
	if err != nil {
		return nil, err
	}
	return &writer{s: s, key: k, f: f, bw: bufio.NewWriterSize(f, writeBufSize), path: path}, nil
}

// UpdateMeta rewrites the .meta file atomically (304 merge); the body is untouched.
func (s *Store) UpdateMeta(_ context.Context, k cachekey.Key, m store.Meta) error {
	pm := s.stripe(k)
	pm.Lock()
	defer pm.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return store.ErrClosed
	}
	fe := s.idx[k]
	if fe == nil {
		s.mu.Unlock()
		return store.ErrMiss
	}
	m.Size = fe.size
	s.mu.Unlock()
	if err := s.writeMeta(k, m); err != nil {
		return err
	}
	s.mu.Lock()
	fe.meta = m
	s.mu.Unlock()
	return nil
}

// Delete unlinks the given keys (tombstoning any that are held) and reports how many existed.
func (s *Store) Delete(_ context.Context, keys ...cachekey.Key) (int, error) {
	deleted := 0
	for _, k := range keys {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return deleted, store.ErrClosed
		}
		fe := s.idx[k]
		if fe != nil {
			s.detachLocked(fe)
			deleted++
		}
		delete(s.vars, k)
		s.mu.Unlock()
		if fe != nil {
			s.reap(fe)
		}
	}
	return deleted, nil
}

// Variants lists the Vary variants recorded under primary, most recent first.
func (s *Store) Variants(_ context.Context, primary cachekey.Key) ([]store.Variant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	vs := s.vars[primary]
	if len(vs) == 0 {
		return nil, nil
	}
	out := make([]store.Variant, len(vs))
	copy(out, vs)
	return out, nil
}

// PutVariant records v under primary, evicting the LRU variant (and its entry) past MaxVariants.
func (s *Store) PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return store.ErrClosed
	}
	evicted, ok := s.putVariantLocked(primary, v)
	s.mu.Unlock()
	if ok {
		s.varEvictions.Add(1)
		if n, err := s.Delete(ctx, evicted.Key); err == nil && n > 0 {
			s.evictions.Add(int64(n))
		}
	}
	return nil
}

// putVariantLocked inserts v at the front (dropping a duplicate key) and
// returns the variant pushed past MaxVariants, if any.
func (s *Store) putVariantLocked(primary cachekey.Key, v store.Variant) (store.Variant, bool) {
	vs := s.vars[primary]
	for i := range vs {
		if vs[i].Key == v.Key {
			vs = append(vs[:i], vs[i+1:]...)
			break
		}
	}
	vs = append(vs, store.Variant{})
	copy(vs[1:], vs)
	vs[0] = v
	var evicted store.Variant
	ok := false
	if len(vs) > s.cfg.MaxVariants {
		evicted, ok = vs[len(vs)-1], true
		vs = vs[:len(vs)-1]
	}
	s.vars[primary] = vs
	return evicted, ok
}

// Stats returns the tier's gauges keyed by store.TierDisk. Bytes includes
// tombstones and in-flight commits.
func (s *Store) Stats() map[string]store.Stats {
	s.mu.Lock()
	b, n := s.bytes, int64(len(s.idx))
	s.mu.Unlock()
	return map[string]store.Stats{store.TierDisk: {
		Bytes:     b,
		Entries:   n,
		Evictions: s.evictions.Load(),
		Capacity:  s.cfg.Capacity,
	}}
}

// Walk calls fn for every indexed entry's key and meta (used to rebuild the tag
// index after scan recovery); a non-nil error from fn stops the walk. It
// snapshots the index first, so fn may call back into the store.
func (s *Store) Walk(ctx context.Context, fn func(cachekey.Key, store.Meta) error) error {
	type kv struct {
		k cachekey.Key
		m store.Meta
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return store.ErrClosed
	}
	snap := make([]kv, 0, len(s.idx))
	for k, fe := range s.idx {
		snap = append(snap, kv{k, fe.meta})
	}
	s.mu.Unlock()
	for i := range snap {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(snap[i].k, snap[i].m); err != nil {
			return err
		}
	}
	return nil
}

// Sweep is the janitor hook: it retries tombstoned unlinks and removes stale
// temp files, returning how many tombstones and temp files it freed. The
// engine calls it every EDGECACHE_JANITOR_INTERVAL.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, store.ErrClosed
	}
	tombs := make([]*fileEntry, 0, len(s.tombs))
	for _, fe := range s.tombs {
		tombs = append(tombs, fe)
	}
	s.mu.Unlock()
	freed := 0
	for _, fe := range tombs {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		if s.reap(fe) {
			freed++
		}
	}
	files, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return freed, fmt.Errorf("diskstore: sweep %s: %w", s.tmpDir, err)
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		s.mu.Lock()
		_, live := s.temps[f.Name()]
		s.mu.Unlock()
		if live {
			continue
		}
		if err := os.Remove(filepath.Join(s.tmpDir, f.Name())); err == nil {
			freed++
		}
	}
	s.swept.Add(int64(freed))
	return freed, nil
}

// Close releases every unheld tombstone; further calls return store.ErrClosed.
// Metas are written synchronously, so there is nothing to flush. Entries still
// held by readers stay readable and are unlinked as they are released.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return store.ErrClosed
	}
	s.closed = true
	tombs := make([]*fileEntry, 0, len(s.tombs))
	for _, fe := range s.tombs {
		tombs = append(tombs, fe)
	}
	s.vars = make(map[cachekey.Key][]store.Variant)
	s.mu.Unlock()
	for _, fe := range tombs {
		s.reap(fe)
	}
	return nil
}

// --- paths and temp files ---

func (s *Store) stripe(k cachekey.Key) *sync.Mutex { return &s.pub[int(k[1])&(pubStripes-1)] }

func (s *Store) pairBase(k cachekey.Key) string {
	h := k.String()
	return filepath.Join(s.cfg.Dir, h[:2], h)
}

func (s *Store) bodyPath(k cachekey.Key) string { return s.pairBase(k) + bodyExt }
func (s *Store) metaPath(k cachekey.Key) string { return s.pairBase(k) + metaExt }

// createTemp opens a fresh temp file for k and registers it so Sweep leaves it alone.
func (s *Store) createTemp(k cachekey.Key, ext string) (*os.File, string, error) {
	name := k.String() + "." + strconv.FormatUint(s.seq.Add(1), 10) + ext
	path := filepath.Join(s.tmpDir, name)
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return nil, "", fmt.Errorf("diskstore: create temp: %w", err)
	}
	s.mu.Lock()
	s.temps[name] = struct{}{}
	s.mu.Unlock()
	return f, path, nil
}

// forgetTemp unregisters a temp file; it is called once the file is renamed or removed.
func (s *Store) forgetTemp(path string) {
	s.mu.Lock()
	delete(s.temps, filepath.Base(path))
	s.mu.Unlock()
}

// discardTemp removes a temp file and unregisters it.
func (s *Store) discardTemp(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Debug("diskstore: remove temp", "path", path, "err", err)
	}
	s.forgetTemp(path)
}

// clearTemps empties the temp directory at start (no writer can own anything yet).
func (s *Store) clearTemps() {
	files, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return
	}
	for _, f := range files {
		_ = os.Remove(filepath.Join(s.tmpDir, f.Name()))
	}
}

// writeMeta publishes m for k atomically: temp, Sync, rename. Callers hold the key's stripe.
func (s *Store) writeMeta(k cachekey.Key, m store.Meta) error {
	b, err := encodeMeta(k, m)
	if err != nil {
		return err
	}
	f, tmp, err := s.createTemp(k, metaExt)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.discardTemp(tmp)
		return fmt.Errorf("diskstore: write meta: %w", err)
	}
	dst := s.metaPath(k)
	err = os.MkdirAll(filepath.Dir(dst), dirPerm)
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		s.discardTemp(tmp)
		return fmt.Errorf("diskstore: publish meta: %w", err)
	}
	s.forgetTemp(tmp)
	return nil
}

// unlinkPair removes k's files, meta first so a crash leaves a sweepable orphan
// body rather than a meta without a body. A missing file is not an error.
func (s *Store) unlinkPair(k cachekey.Key) error {
	for _, p := range []string{s.metaPath(k), s.bodyPath(k)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// --- index maintenance ---

// detachLocked takes fe out of the index and onto the tombstone list; its
// bytes stay counted until reap unlinks the files.
func (s *Store) detachLocked(fe *fileEntry) {
	delete(s.idx, fe.key)
	if fe.elem != nil {
		s.lru.Remove(fe.elem)
		fe.elem = nil
	}
	fe.state = stateEvicted
	s.tombs[fe.key] = fe
}

// retireLocked marks fe replaced by a newer generation: no unlink (the path
// now belongs to the new files), bytes released now or with the last reader.
func (s *Store) retireLocked(fe *fileEntry) {
	if fe.state == stateEvicted {
		delete(s.tombs, fe.key)
	}
	fe.state = stateReplaced
	if fe.refs == 0 {
		s.freeLocked(fe)
	}
}

func (s *Store) freeLocked(fe *fileEntry) {
	if fe.state == stateFreed {
		return
	}
	fe.state = stateFreed
	s.bytes -= fe.size
}

// evictLocked detaches LRU tails until the bytes that will be freed bring the
// total to limit or the list is empty; held tails become tombstones (nothing
// freed now). It returns the victims and the projected byte total.
func (s *Store) evictLocked(limit int64) (victims []*fileEntry, projected int64) {
	projected = s.bytes
	for e := s.lru.Back(); e != nil && projected > limit; {
		prev := e.Prev()
		fe, ok := e.Value.(*fileEntry)
		if !ok {
			e = prev
			continue
		}
		s.detachLocked(fe)
		s.evictions.Add(1)
		if fe.refs == 0 {
			projected -= fe.size
		}
		victims = append(victims, fe)
		e = prev
	}
	return victims, projected
}

func (s *Store) reapAll(victims []*fileEntry) {
	for _, fe := range victims {
		s.reap(fe)
	}
}

// reap unlinks a tombstone's files and releases its bytes. It is a no-op while
// the entry is held or once a newer generation owns its path; an unlink the OS
// refuses (Windows: a reader still has the file open) leaves the tombstone for
// the next Sweep.
func (s *Store) reap(fe *fileEntry) bool {
	pm := s.stripe(fe.key)
	pm.Lock()
	defer pm.Unlock()
	s.mu.Lock()
	if s.tombs[fe.key] != fe || fe.refs > 0 {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	err := s.unlinkPair(fe.key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.deferred.Add(1)
		s.log.Debug("diskstore: unlink deferred to sweep", "key", fe.key.String(), "err", err)
		return false
	}
	delete(s.tombs, fe.key)
	s.freeLocked(fe)
	return true
}

// publish moves the finished temp body into place, writes the meta last and
// indexes the entry, returning it held. It owns tmp: on any failure the temp is
// removed and the reserved bytes returned.
func (s *Store) publish(k cachekey.Key, m store.Meta, tmp string, size int64) (*store.Entry, error) {
	m.Size = size
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.discardTemp(tmp)
		return nil, store.ErrClosed
	}
	victims, projected := s.evictLocked(s.cfg.Capacity - size)
	if projected+size > s.cfg.Capacity+s.overdraft {
		s.mu.Unlock()
		s.reapAll(victims)
		s.discardTemp(tmp)
		return nil, fmt.Errorf("diskstore: no room for %d bytes (held entries pin the budget): %w", size, store.ErrTooLarge)
	}
	s.bytes += size // reserved until indexed or returned below
	s.mu.Unlock()
	s.reapAll(victims)

	pm := s.stripe(k)
	pm.Lock()
	defer pm.Unlock()

	// Take the previous generation out of the index for the swap window so a
	// concurrent Get cannot pair its meta with the new body.
	s.mu.Lock()
	old := s.idx[k]
	if old != nil {
		delete(s.idx, k)
		s.lru.Remove(old.elem)
		old.elem = nil
	}
	_, tombstoned := s.tombs[k]
	s.mu.Unlock()

	dst := s.bodyPath(k)
	err := os.MkdirAll(filepath.Dir(dst), dirPerm)
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		s.discardTemp(tmp)
		s.mu.Lock()
		if old != nil {
			old.elem = s.lru.PushFront(old)
			s.idx[k] = old
		}
		s.bytes -= size
		s.mu.Unlock()
		if old != nil || tombstoned {
			return nil, fmt.Errorf("diskstore: publish %s: %w: %w", k.String(), ErrBusy, err)
		}
		return nil, fmt.Errorf("diskstore: publish %s: %w", k.String(), err)
	}
	s.forgetTemp(tmp)

	if err := s.writeMeta(k, m); err != nil {
		// The body is in place under a stale or missing meta: drop the pair so
		// neither the index nor the next scan can serve it.
		if uerr := s.unlinkPair(k); uerr != nil {
			s.log.Warn("diskstore: orphaned pair after meta failure", "key", k.String(), "err", uerr)
		}
		s.mu.Lock()
		s.bytes -= size
		s.retireGenerationsLocked(k, old)
		s.mu.Unlock()
		return nil, err
	}

	s.mu.Lock()
	s.retireGenerationsLocked(k, old)
	fe := &fileEntry{key: k, meta: m, size: size, refs: 1, state: stateLive, s: s}
	fe.bind()
	fe.elem = s.lru.PushFront(fe)
	s.idx[k] = fe
	s.mu.Unlock()
	return store.NewEntry(k, m, store.TierDisk, fe.openFn, fe.releaseFn), nil
}

// retireGenerationsLocked marks the previous live entry and any tombstone for
// k as replaced: their files were just overwritten, so they must never be
// unlinked or opened again.
func (s *Store) retireGenerationsLocked(k cachekey.Key, old *fileEntry) {
	if old != nil {
		s.retireLocked(old)
	}
	if t := s.tombs[k]; t != nil {
		s.retireLocked(t)
	}
}

// --- entry callbacks ---

// open checks the generation and opens the body under the key's stripe lock,
// the same lock publish holds across its rename + retire: without it a reader
// could see stateLive, lose the race to the rename and open the NEW body under
// the old meta (seen once in 10k concurrent reads on Linux, where the rename
// over an open path succeeds).
func (fe *fileEntry) open() (io.ReadCloser, error) {
	s := fe.s
	pm := s.stripe(fe.key)
	pm.Lock()
	defer pm.Unlock()
	s.mu.Lock()
	st := fe.state
	s.mu.Unlock()
	if st == stateReplaced || st == stateFreed {
		return nil, fmt.Errorf("diskstore: body replaced: %w", store.ErrMiss)
	}
	f, err := os.Open(s.bodyPath(fe.key))
	if err != nil {
		return nil, fmt.Errorf("diskstore: open body: %w", err)
	}
	return f, nil
}

func (fe *fileEntry) release() {
	s := fe.s
	s.mu.Lock()
	fe.refs--
	if fe.refs > 0 {
		s.mu.Unlock()
		return
	}
	switch fe.state {
	case stateReplaced:
		s.freeLocked(fe)
		s.mu.Unlock()
	case stateEvicted:
		s.mu.Unlock()
		s.reap(fe)
	case stateLive, stateFreed:
		s.mu.Unlock()
	}
}

// --- writer ---

// writer streams to a temp file with a running size check and publishes on Commit.
type writer struct {
	s    *Store
	key  cachekey.Key
	f    *os.File
	bw   *bufio.Writer
	path string
	n    int64
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
	if w.n+int64(len(p)) > w.s.cfg.MaxObjectBytes {
		w.err = fmt.Errorf("diskstore: body over the %d byte cap: %w", w.s.cfg.MaxObjectBytes, store.ErrTooLarge)
		w.discard()
		return 0, w.err
	}
	n, err := w.bw.Write(p)
	w.n += int64(n)
	if err != nil {
		w.err = fmt.Errorf("diskstore: write temp: %w", err)
		w.discard()
		return n, w.err
	}
	return n, nil
}

// Commit flushes and fsyncs the temp body, renames it into place, writes the
// meta last and returns a held entry.
func (w *writer) Commit(final store.Meta) (*store.Entry, error) {
	if w.done {
		return nil, ErrCommitted
	}
	w.done = true
	if w.err != nil {
		return nil, w.err
	}
	err := w.bw.Flush()
	if err == nil {
		err = w.f.Sync()
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f = nil
	if err != nil {
		w.s.discardTemp(w.path)
		return nil, fmt.Errorf("diskstore: finish temp: %w", err)
	}
	return w.s.publish(w.key, final, w.path, w.n)
}

// Abort discards the temp file; it is idempotent and a no-op after Commit.
func (w *writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	w.discard()
	return nil
}

func (w *writer) discard() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.s.discardTemp(w.path)
}

// --- recovery ---

// scan rebuilds the index from <dir>/<aa>/*.meta: orphan bodies are swept,
// corrupt pairs removed, the LRU ordered by meta mtime, variant lists rebuilt
// from each meta's Vary, and the total trimmed to Capacity.
func (s *Store) scan() error {
	subs, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return fmt.Errorf("diskstore: scan %s: %w", s.cfg.Dir, err)
	}
	type rec struct {
		fe    *fileEntry
		mtime time.Time
	}
	var recs []rec
	for _, sub := range subs {
		if !sub.IsDir() || len(sub.Name()) != 2 || !isHex(sub.Name()) {
			continue
		}
		dir := filepath.Join(s.cfg.Dir, sub.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			s.log.Warn("diskstore: scan skipped directory", "path", dir, "err", err)
			continue
		}
		bodies := make(map[string]struct{})
		metas := make(map[string]fs.DirEntry)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			switch name := f.Name(); {
			case strings.HasSuffix(name, metaExt):
				metas[strings.TrimSuffix(name, metaExt)] = f
			case strings.HasSuffix(name, bodyExt):
				bodies[strings.TrimSuffix(name, bodyExt)] = struct{}{}
			}
		}
		for base := range bodies {
			if _, ok := metas[base]; ok {
				continue
			}
			p := filepath.Join(dir, base+bodyExt)
			s.log.Warn("diskstore: sweeping orphan body", "path", p)
			_ = os.Remove(p)
			s.orphans.Add(1)
		}
		for base, mf := range metas {
			metaPath := filepath.Join(dir, base+metaExt)
			bodyPath := filepath.Join(dir, base+bodyExt)
			k, m, size, err := s.loadPair(sub.Name(), base, metaPath, bodyPath)
			if err != nil {
				s.log.Warn("diskstore: dropping corrupt entry", "path", metaPath, "err", err)
				_ = os.Remove(metaPath)
				_ = os.Remove(bodyPath)
				s.corrupt.Add(1)
				continue
			}
			var mtime time.Time
			if info, err := mf.Info(); err == nil {
				mtime = info.ModTime()
			}
			fe := &fileEntry{key: k, meta: m, size: size, state: stateLive, s: s}
			fe.bind()
			recs = append(recs, rec{fe: fe, mtime: mtime})
		}
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].mtime.Before(recs[j].mtime) })
	s.mu.Lock()
	for _, r := range recs {
		fe := r.fe
		fe.elem = s.lru.PushFront(fe)
		s.idx[fe.key] = fe
		s.bytes += fe.size
		s.addVariantLocked(fe)
	}
	var overflow []store.Variant
	for p, vs := range s.vars {
		sort.SliceStable(vs, func(i, j int) bool { return vs[i].Seen.After(vs[j].Seen) })
		if len(vs) > s.cfg.MaxVariants {
			overflow = append(overflow, vs[s.cfg.MaxVariants:]...)
			vs = vs[:s.cfg.MaxVariants]
		}
		s.vars[p] = vs
	}
	victims, _ := s.evictLocked(s.cfg.Capacity)
	s.mu.Unlock()
	s.reapAll(victims)
	for _, v := range overflow {
		s.varEvictions.Add(1)
		if n, err := s.Delete(context.Background(), v.Key); err == nil && n > 0 {
			s.evictions.Add(int64(n))
		}
	}
	return nil
}

// addVariantLocked re-registers a Vary-keyed entry under its primary key so
// Variants works after a restart; Seen is the response time.
func (s *Store) addVariantLocked(fe *fileEntry) {
	m := fe.meta
	if len(m.Vary) == 0 || m.Method == "" || m.URL == "" {
		return
	}
	u, err := url.Parse(m.URL)
	if err != nil {
		return
	}
	p := cachekey.Primary(m.Method, u)
	s.vars[p] = append(s.vars[p], store.Variant{Vary: m.Vary, Key: fe.key, Seen: m.ResponseTime})
}

// loadPair validates one <aa>/<base>.meta + .body pair and returns its key,
// meta and body size.
func (s *Store) loadPair(sub, base, metaPath, bodyPath string) (cachekey.Key, store.Meta, int64, error) {
	var k cachekey.Key
	raw, err := hex.DecodeString(base)
	if err != nil || len(raw) != len(k) {
		return k, store.Meta{}, 0, errors.New("file name is not a key")
	}
	copy(k[:], raw)
	if base[:2] != sub {
		return k, store.Meta{}, 0, errors.New("key does not belong to this directory")
	}
	b, err := os.ReadFile(filepath.Clean(metaPath))
	if err != nil {
		return k, store.Meta{}, 0, fmt.Errorf("read meta: %w", err)
	}
	m, err := decodeMeta(b, base)
	if err != nil {
		return k, store.Meta{}, 0, err
	}
	info, err := os.Stat(bodyPath)
	if err != nil {
		return k, store.Meta{}, 0, fmt.Errorf("stat body: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != m.Size {
		return k, store.Meta{}, 0, fmt.Errorf("body is %d bytes, meta says %d", info.Size(), m.Size)
	}
	s.log.Debug("diskstore: recovered", "key", base, "size", info.Size())
	return k, m, info.Size(), nil
}

// maxMetaBytes bounds a .meta file: a stored header block is at most
// http.DefaultMaxHeaderBytes, and the rest of Meta is small, so anything
// larger is corrupt (or hostile) and is dropped before being decoded.
const maxMetaBytes = 2 << 20

// encodeMeta is the on-disk form of a meta: the metaFile JSON envelope.
func encodeMeta(k cachekey.Key, m store.Meta) ([]byte, error) {
	b, err := json.Marshal(metaFile{Version: metaVersion, Key: k.String(), Meta: m})
	if err != nil {
		return nil, fmt.Errorf("diskstore: encode meta: %w", err)
	}
	return b, nil
}

// decodeMeta parses a .meta file for the key named by base (the file's hex
// stem) and validates the envelope: size cap, version, key, a non-negative
// body size and a status a response can carry. Unknown fields are rejected so
// a foreign JSON document never becomes an entry.
func decodeMeta(b []byte, base string) (store.Meta, error) {
	if len(b) > maxMetaBytes {
		return store.Meta{}, fmt.Errorf("meta is %d bytes, cap %d", len(b), maxMetaBytes)
	}
	var mf metaFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mf); err != nil {
		return store.Meta{}, fmt.Errorf("decode meta: %w", err)
	}
	if dec.More() {
		return store.Meta{}, errors.New("decode meta: trailing data")
	}
	if mf.Version != metaVersion {
		return store.Meta{}, fmt.Errorf("meta version %d, want %d", mf.Version, metaVersion)
	}
	if mf.Key != base {
		return store.Meta{}, fmt.Errorf("meta names key %q", mf.Key)
	}
	if mf.Meta.Size < 0 {
		return store.Meta{}, fmt.Errorf("meta size %d is negative", mf.Meta.Size)
	}
	if mf.Meta.Status < 100 || mf.Meta.Status > 999 {
		return store.Meta{}, fmt.Errorf("meta status %d is not an HTTP status", mf.Meta.Status)
	}
	return mf.Meta, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
