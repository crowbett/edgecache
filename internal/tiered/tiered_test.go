package tiered

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/diskstore"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/store"
)

func key(i int) cachekey.Key {
	var k cachekey.Key
	k[0] = byte(i)
	k[1] = byte(i >> 8)
	return k
}

type fixture struct {
	s    *Store
	mem  *memstore.Store
	disk *diskstore.Store
}

// memMax is the memory admission cap every fixture uses.
const memMax = 100

// newFixture builds a real memory tier (memMax admission) in front of a real
// disk tier in a temp dir; withDisk=false gives memory-only.
func newFixture(t *testing.T, withDisk bool) fixture {
	t.Helper()
	mem, err := memstore.New(memstore.Config{Capacity: 1 << 20, MaxObjectBytes: memMax})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{mem: mem}
	if withDisk {
		f.disk, err = diskstore.New(diskstore.Config{Dir: t.TempDir(), Capacity: 1 << 20}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		f.s = New(Config{MemMaxObjectBytes: memMax}, mem, f.disk)
	} else {
		f.s = New(Config{MemMaxObjectBytes: memMax}, mem, nil)
	}
	t.Cleanup(func() { _ = f.s.Close() })
	return f
}

func put(t *testing.T, s *Store, k cachekey.Key, body []byte) *store.Entry {
	t.Helper()
	w, err := s.Writer(context.Background(), k, store.Meta{Size: int64(len(body))})
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	e, err := w.Commit(store.Meta{URL: "u", Size: int64(len(body))})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return e
}

func readBody(t *testing.T, e *store.Entry) []byte {
	t.Helper()
	rc, err := e.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return b
}

func tierHas(t *testing.T, tier Tier, k cachekey.Key) bool {
	t.Helper()
	e, err := tier.Get(context.Background(), k)
	if errors.Is(err, store.ErrMiss) {
		return false
	}
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	e.Release()
	return true
}

func TestFanOutAndPromotion(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	body := bytes.Repeat([]byte{1}, 50)

	// Small body: both tiers, the memory entry comes back.
	e := put(t, f.s, key(1), body)
	if e.Tier != store.TierMem {
		t.Errorf("commit returned tier %q, want mem", e.Tier)
	}
	if b := readBody(t, e); !bytes.Equal(b, body) {
		t.Errorf("body = %v", b)
	}
	e.Release()
	if !tierHas(t, f.mem, key(1)) || !tierHas(t, f.disk, key(1)) {
		t.Error("small body not in both tiers")
	}

	// Large body: disk only, memory half dropped by the running size check.
	big := bytes.Repeat([]byte{2}, 150)
	w, err := f.s.Writer(ctx, key(2), store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Write(big[i*50 : (i+1)*50]); err != nil {
			t.Fatal(err)
		}
	}
	e, err = w.Commit(store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Tier != store.TierDisk || e.Meta.Size != 150 {
		t.Errorf("large commit = %s/%d", e.Tier, e.Meta.Size)
	}
	e.Release()
	if tierHas(t, f.mem, key(2)) || !tierHas(t, f.disk, key(2)) {
		t.Error("large body should be disk only")
	}
	// Declared-large: the memory writer is never opened.
	put(t, f.s, key(3), big).Release()
	if tierHas(t, f.mem, key(3)) {
		t.Error("declared-large body reached memory")
	}

	// Promotion: evict key 1 from memory, then a tiered Get serves disk and promotes.
	if _, err := f.mem.Delete(ctx, key(1)); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Get(ctx, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != store.TierMem || got.Meta.Size != 50 {
		t.Errorf("promoted Get = %s/%d", got.Tier, got.Meta.Size)
	}
	if b := readBody(t, got); !bytes.Equal(b, body) {
		t.Errorf("promoted body = %v", b)
	}
	got.Release()
	if !tierHas(t, f.mem, key(1)) {
		t.Error("disk hit not promoted into memory")
	}
	if c := f.s.Counters(); c.Promotions != 1 || c.PromotionFailures != 0 {
		t.Errorf("Counters = %+v", c)
	}
	// A large disk hit is served from disk, not promoted.
	got, err = f.s.Get(ctx, key(2))
	if err != nil || got.Tier != store.TierDisk {
		t.Fatalf("large Get = %v, %v", got, err)
	}
	got.Release()
	if tierHas(t, f.mem, key(2)) || f.s.Counters().Promotions != 1 {
		t.Error("large body promoted")
	}
	if _, err := f.s.Get(ctx, key(9)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("miss = %v", err)
	}

	// UpdateMeta and Delete reach both tiers.
	if err := f.s.UpdateMeta(ctx, key(1), store.Meta{URL: "v"}); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []Tier{f.mem, f.disk} {
		e, _ := tier.Get(ctx, key(1))
		if e.Meta.URL != "v" || e.Meta.Size != 50 {
			t.Errorf("UpdateMeta not applied in %s: %+v", e.Tier, e.Meta)
		}
		e.Release()
	}
	if err := f.s.UpdateMeta(ctx, key(2), store.Meta{URL: "w"}); err != nil {
		t.Errorf("UpdateMeta disk-only entry = %v", err)
	}
	if err := f.s.UpdateMeta(ctx, key(9), store.Meta{}); !errors.Is(err, store.ErrMiss) {
		t.Errorf("UpdateMeta missing = %v", err)
	}
	n, err := f.s.Delete(ctx, key(1), key(2), key(9))
	if err != nil || n != 2 {
		t.Errorf("Delete = %d, %v", n, err)
	}
	for _, k := range []cachekey.Key{key(1), key(2)} {
		if tierHas(t, f.mem, k) || tierHas(t, f.disk, k) {
			t.Errorf("key %x survived Delete", k[:2])
		}
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	w, err := f.s.Writer(ctx, key(1), store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if tierHas(t, f.mem, key(1)) || tierHas(t, f.disk, key(1)) {
		t.Error("visible before Commit")
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal("second Abort:", err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, ErrCommitted) {
		t.Errorf("Commit after Abort = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrCommitted) {
		t.Errorf("Write after Abort = %v", err)
	}
	if tierHas(t, f.mem, key(1)) || tierHas(t, f.disk, key(1)) {
		t.Error("Abort left a tier populated")
	}
	st := f.s.Stats()
	if st[store.TierMem].Bytes != 0 || st[store.TierDisk].Bytes != 0 || st[store.TierMem].Entries != 0 || st[store.TierDisk].Entries != 0 {
		t.Errorf("Stats after Abort = %+v", st)
	}

	// Abort after Commit is a no-op; the entry stays.
	e := put(t, f.s, key(2), []byte("kept"))
	e.Release()
	w, _ = f.s.Writer(ctx, key(3), store.Meta{})
	_, _ = w.Write([]byte("x"))
	e, err = w.Commit(store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	e.Release()
	if err := w.Abort(); err != nil {
		t.Errorf("Abort after Commit = %v", err)
	}
	if !tierHas(t, f.mem, key(3)) || !tierHas(t, f.disk, key(3)) {
		t.Error("Abort after Commit removed the entry")
	}
}

func TestMemoryOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()
	e := put(t, f.s, key(1), []byte("mem"))
	if e.Tier != store.TierMem {
		t.Errorf("tier = %s", e.Tier)
	}
	e.Release()
	got, err := f.s.Get(ctx, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if b := readBody(t, got); string(b) != "mem" {
		t.Errorf("body = %q", b)
	}
	got.Release()
	// Over the memory cap with no disk: refused, not silently dropped.
	if _, err := f.s.Writer(ctx, key(2), store.Meta{Size: 101}); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Writer over cap = %v", err)
	}
	w, _ := f.s.Writer(ctx, key(2), store.Meta{})
	if _, err := w.Write(make([]byte, 101)); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Write over cap = %v", err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Commit over cap = %v", err)
	}
	if _, err := f.s.Get(ctx, key(2)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("over-cap body stored: %v", err)
	}
	if n, err := f.s.Sweep(ctx); n != 0 || err != nil {
		t.Errorf("Sweep without disk = %d, %v", n, err)
	}
	seen := 0
	if err := f.s.Walk(ctx, func(cachekey.Key, store.Meta) error { seen++; return nil }); err != nil || seen != 1 {
		t.Errorf("Walk memory-only: seen=%d err=%v", seen, err)
	}
	st := f.s.Stats()
	if _, ok := st[store.TierDisk]; ok {
		t.Error("disk stats reported without a disk tier")
	}
	if st[store.TierMem].Entries != 1 || st[store.TierMem].Bytes != 3 {
		t.Errorf("mem stats = %+v", st[store.TierMem])
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Get(ctx, key(1)); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Get after Close = %v", err)
	}
}

func TestStatsVariantsWalkSweep(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	put(t, f.s, key(1), make([]byte, 10)).Release()  // both tiers
	put(t, f.s, key(2), make([]byte, 150)).Release() // disk only

	st := f.s.Stats()
	if st[store.TierMem].Entries != 1 || st[store.TierMem].Bytes != 10 || st[store.TierMem].Capacity != 1<<20 {
		t.Errorf("mem stats = %+v", st[store.TierMem])
	}
	if st[store.TierDisk].Entries != 2 || st[store.TierDisk].Bytes != 160 || st[store.TierDisk].Capacity != 1<<20 {
		t.Errorf("disk stats = %+v", st[store.TierDisk])
	}

	primary := key(100)
	v1 := store.Variant{Vary: []string{"accept"}, Key: key(1), Seen: time.Unix(1, 0)}
	if err := f.s.PutVariant(ctx, primary, v1); err != nil {
		t.Fatal(err)
	}
	// A newer sighting recorded only in one tier wins the merge.
	v1b := v1
	v1b.Seen = time.Unix(5, 0)
	if err := f.disk.PutVariant(ctx, primary, v1b); err != nil {
		t.Fatal(err)
	}
	v2 := store.Variant{Vary: []string{"accept"}, Key: key(2), Seen: time.Unix(3, 0)}
	if err := f.mem.PutVariant(ctx, primary, v2); err != nil {
		t.Fatal(err)
	}
	vs, err := f.s.Variants(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].Key != key(1) || vs[0].Seen.Unix() != 5 || vs[1].Key != key(2) {
		t.Errorf("merged variants = %v", vs)
	}

	// Walk enumerates the disk tier (memory is a subset).
	seen := map[cachekey.Key]int64{}
	if err := f.s.Walk(ctx, func(k cachekey.Key, m store.Meta) error { seen[k] = m.Size; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[key(1)] != 10 || seen[key(2)] != 150 {
		t.Errorf("walked %v", seen)
	}
	stop := errors.New("stop")
	if err := f.s.Walk(ctx, func(cachekey.Key, store.Meta) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("Walk stop = %v", err)
	}
	if _, err := f.s.Sweep(ctx); err != nil {
		t.Errorf("Sweep = %v", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); !errors.Is(err, store.ErrClosed) {
		t.Errorf("second Close = %v", err)
	}
}
