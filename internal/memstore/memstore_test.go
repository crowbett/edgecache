package memstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

func key(i int) cachekey.Key {
	var k cachekey.Key
	k[0] = byte(i)
	k[1] = byte(i >> 8)
	k[2] = byte(i >> 16)
	return k
}

func put(t *testing.T, s *Store, k cachekey.Key, body []byte) *store.Entry {
	t.Helper()
	w, err := s.Writer(context.Background(), k, store.Meta{})
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

// audit recomputes every shard's byte total from its index and checks it
// against the global counter (valid once no detached entry is held).
func audit(t *testing.T, s *Store) {
	t.Helper()
	var sum int64
	for _, sh := range s.shards {
		sh.mu.Lock()
		var b int64
		for _, n := range sh.m {
			b += n.size
			if n.detached {
				t.Errorf("detached node %x still indexed", n.key[:3])
			}
		}
		if b != sh.bytes {
			t.Errorf("shard bytes %d != indexed %d", sh.bytes, b)
		}
		sum += b
		sh.mu.Unlock()
	}
	if got := s.total.Load(); got != sum {
		t.Errorf("total %d != sum of shards %d", got, sum)
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"defaults", Config{Capacity: 1 << 20}, false},
		{"zero capacity", Config{}, true},
		{"non power of two", Config{Capacity: 1, Shards: 3}, true},
		{"too many shards", Config{Capacity: 1, Shards: 512}, true},
		{"one shard", Config{Capacity: 1, Shards: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%+v) err = %v, wantErr %v", tc.cfg, err, tc.wantErr)
			}
			if err == nil {
				if s.cfg.Shards == 0 || s.cfg.MaxVariants == 0 || s.cfg.MaxObjectBytes == 0 {
					t.Errorf("defaults not applied: %+v", s.cfg)
				}
			}
		})
	}
}

func TestRoundTripAndStats(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Fatalf("Get empty = %v, want ErrMiss", err)
	}
	body := []byte("hello world")
	e := put(t, s, key(1), body)
	if e.Tier != store.TierMem || e.Meta.Size != int64(len(body)) || e.Meta.URL != "u" {
		t.Errorf("entry = %+v", e)
	}
	if got := readBody(t, e); !bytes.Equal(got, body) {
		t.Errorf("body = %q", got)
	}
	e.Release()

	got, err := s.Get(ctx, key(1))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b := readBody(t, got); !bytes.Equal(b, body) {
		t.Errorf("Get body = %q", b)
	}
	got.Release()

	if err := s.UpdateMeta(ctx, key(1), store.Meta{URL: "v", ETag: `"x"`, Size: 999}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	got, _ = s.Get(ctx, key(1))
	if got.Meta.URL != "v" || got.Meta.ETag != `"x"` || got.Meta.Size != int64(len(body)) {
		t.Errorf("after UpdateMeta meta = %+v", got.Meta)
	}
	if b := readBody(t, got); !bytes.Equal(b, body) {
		t.Errorf("body after UpdateMeta = %q", b)
	}
	got.Release()
	if err := s.UpdateMeta(ctx, key(2), store.Meta{}); !errors.Is(err, store.ErrMiss) {
		t.Errorf("UpdateMeta missing = %v", err)
	}

	st := s.Stats()[store.TierMem]
	if st.Entries != 1 || st.Bytes != int64(len(body)) || st.Capacity != 1<<20 || st.Evictions != 0 {
		t.Errorf("Stats = %+v", st)
	}
	c := s.Counters()
	if c.Hits != 2 || c.Misses != 1 {
		t.Errorf("Counters = %+v", c)
	}

	n, err := s.Delete(ctx, key(1), key(2))
	if err != nil || n != 1 {
		t.Errorf("Delete = %d, %v", n, err)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("Get after Delete = %v", err)
	}
	if st := s.Stats()[store.TierMem]; st.Entries != 0 || st.Bytes != 0 {
		t.Errorf("Stats after Delete = %+v", st)
	}
	audit(t, s)
}

func TestCommitAbort(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Capacity: 1 << 20, MaxObjectBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Abort discards; nothing published.
	w, _ := s.Writer(ctx, key(1), store.Meta{})
	_, _ = w.Write([]byte("abc"))
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal("second Abort", err)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("after Abort Get = %v", err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, ErrCommitted) {
		t.Errorf("Commit after Abort = %v", err)
	}

	// Nothing is visible until Commit.
	w, _ = s.Writer(ctx, key(2), store.Meta{})
	_, _ = w.Write([]byte("partial"))
	if _, err := s.Get(ctx, key(2)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("visible before Commit: %v", err)
	}
	e, err := w.Commit(store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	e.Release()
	if err := w.Abort(); err != nil {
		t.Errorf("Abort after Commit = %v", err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, ErrCommitted) {
		t.Errorf("double Commit = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrCommitted) {
		t.Errorf("Write after Commit = %v", err)
	}
	if e2, err := s.Get(ctx, key(2)); err != nil {
		t.Errorf("Get after Commit = %v", err)
	} else {
		if b := readBody(t, e2); string(b) != "partial" {
			t.Errorf("body = %q", b)
		}
		e2.Release()
	}

	// Admission: declared size and streamed size past the cap.
	if _, err := s.Writer(ctx, key(3), store.Meta{Size: 101}); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Writer declared too large = %v", err)
	}
	w, _ = s.Writer(ctx, key(3), store.Meta{})
	if _, err := w.Write(make([]byte, 60)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 41)); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Write over cap = %v", err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("Commit over cap = %v", err)
	}
	if _, err := s.Get(ctx, key(3)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("over-cap body published: %v", err)
	}

	// Overwrite replaces the body atomically.
	put(t, s, key(2), []byte("second")).Release()
	e3, _ := s.Get(ctx, key(2))
	if b := readBody(t, e3); string(b) != "second" {
		t.Errorf("overwritten body = %q", b)
	}
	e3.Release()
	if st := s.Stats()[store.TierMem]; st.Entries != 1 || st.Bytes != 6 {
		t.Errorf("Stats after overwrite = %+v", st)
	}
	audit(t, s)
}

func TestLRUOrder(t *testing.T) {
	t.Parallel()
	// One shard so every key competes for the same budget: 3 x 100 bytes fit, a 4th evicts.
	s, err := New(Config{Capacity: 300, Shards: 1, MaxObjectBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := make([]byte, 100)
	for i := 1; i <= 3; i++ {
		put(t, s, key(i), body).Release()
	}
	e, _ := s.Get(ctx, key(1)) // touch 1: LRU order is now 2,3,1
	e.Release()
	put(t, s, key(4), body).Release()
	if _, err := s.Get(ctx, key(2)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("key 2 should have been evicted, err = %v", err)
	}
	for _, i := range []int{1, 3, 4} {
		e, err := s.Get(ctx, key(i))
		if err != nil {
			t.Errorf("key %d evicted unexpectedly", i)
			continue
		}
		e.Release()
	}
	st := s.Stats()[store.TierMem]
	if st.Bytes != 300 || st.Entries != 3 || st.Evictions != 1 {
		t.Errorf("Stats = %+v", st)
	}
	audit(t, s)
}

func TestHeldEntryNotFreed(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Capacity: 300, Shards: 1, MaxObjectBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := bytes.Repeat([]byte{7}, 100)
	held := put(t, s, key(1), body) // held from Commit
	for i := 2; i <= 3; i++ {
		put(t, s, key(i), body).Release()
	}
	// Two more inserts must evict 2 and 3, never the held 1.
	put(t, s, key(4), body).Release()
	put(t, s, key(5), body).Release()
	again, err := s.Get(ctx, key(1))
	if err != nil {
		t.Fatalf("held entry evicted: %v", err)
	}
	again.Release()
	if b := readBody(t, held); !bytes.Equal(b, body) {
		t.Errorf("held body = %v", b[:4])
	}
	if st := s.Stats()[store.TierMem]; st.Bytes > 300+s.Overdraft() {
		t.Errorf("bytes %d over budget", st.Bytes)
	}

	// Delete a held entry: gone from the index, bytes charged until Release.
	if n, _ := s.Delete(ctx, key(1)); n != 1 {
		t.Fatal("Delete held entry")
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("deleted entry still visible")
	}
	if got := s.total.Load(); got != 300 {
		t.Errorf("bytes after Delete of held = %d, want 300 (still charged)", got)
	}
	if b := readBody(t, held); !bytes.Equal(b, body) {
		t.Errorf("held body after Delete = %v", b[:4])
	}
	held.Release() // the Commit reference
	if got := s.total.Load(); got != 200 {
		t.Errorf("bytes after Release = %d, want 200", got)
	}
	held.Release() // idempotent
	if got := s.total.Load(); got != 200 {
		t.Errorf("bytes after double Release = %d", got)
	}
	audit(t, s)

	// All entries held: a commit past Capacity+Overdraft is refused, not admitted.
	a, _ := s.Get(ctx, key(4))
	b, _ := s.Get(ctx, key(5))
	var extra []*store.Entry
	for i := 10; i < 20; i++ {
		w, _ := s.Writer(ctx, key(i), store.Meta{})
		_, _ = w.Write(body)
		e, err := w.Commit(store.Meta{})
		if err != nil {
			if !errors.Is(err, store.ErrTooLarge) {
				t.Fatalf("Commit err = %v", err)
			}
			break
		}
		extra = append(extra, e)
	}
	if got := s.total.Load(); got > 300+s.Overdraft() {
		t.Errorf("bytes %d over Capacity+Overdraft with everything held", got)
	}
	if len(extra) == 10 {
		t.Errorf("all 10 commits admitted with every entry held")
	}
	for _, e := range append(extra, a, b) {
		e.Release()
	}
	audit(t, s)
}

func TestVariantCap(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Capacity: 1 << 20, MaxVariants: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	primary := key(100)
	if vs, err := s.Variants(ctx, primary); err != nil || vs != nil {
		t.Fatalf("Variants empty = %v, %v", vs, err)
	}
	for i := 1; i <= 3; i++ {
		put(t, s, key(i), []byte("v")).Release()
		v := store.Variant{Vary: []string{"accept"}, Key: key(i), Seen: time.Unix(int64(i), 0)}
		if err := s.PutVariant(ctx, primary, v); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.Variants(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].Key != key(3) || vs[1].Key != key(2) {
		t.Errorf("variants = %v", vs)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("oldest variant's entry not evicted: %v", err)
	}
	c := s.Counters()
	if c.VariantEvictions != 1 || c.Evictions != 1 {
		t.Errorf("Counters = %+v", c)
	}
	// Re-putting an existing variant moves it to the front without eviction.
	if err := s.PutVariant(ctx, primary, store.Variant{Key: key(2), Seen: time.Unix(9, 0)}); err != nil {
		t.Fatal(err)
	}
	vs, _ = s.Variants(ctx, primary)
	if len(vs) != 2 || vs[0].Key != key(2) || vs[0].Seen.Unix() != 9 || vs[1].Key != key(3) {
		t.Errorf("variants after re-put = %v", vs)
	}
	if s.Counters().VariantEvictions != 1 {
		t.Errorf("re-put evicted")
	}
	// Deleting the primary drops its variant list.
	if _, err := s.Delete(ctx, primary); err != nil {
		t.Fatal(err)
	}
	if vs, _ := s.Variants(ctx, primary); len(vs) != 0 {
		t.Errorf("variants after Delete(primary) = %v", vs)
	}
}

func TestWalkAndClose(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		put(t, s, key(i), []byte("x")).Release()
	}
	seen := map[cachekey.Key]bool{}
	if err := s.Walk(ctx, func(k cachekey.Key, m store.Meta) error {
		seen[k] = true
		if m.URL != "u" {
			t.Errorf("meta = %+v", m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 {
		t.Errorf("walked %d entries", len(seen))
	}
	stop := errors.New("stop")
	calls := 0
	if err := s.Walk(ctx, func(cachekey.Key, store.Meta) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Errorf("Walk stop: err=%v calls=%d", err, calls)
	}

	held, _ := s.Get(ctx, key(1))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, store.ErrClosed) {
		t.Errorf("second Close = %v", err)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Get after Close = %v", err)
	}
	if _, err := s.Writer(ctx, key(1), store.Meta{}); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Writer after Close = %v", err)
	}
	if _, err := s.Delete(ctx, key(1)); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Delete after Close = %v", err)
	}
	if got := s.Stats()[store.TierMem]; got.Entries != 0 || got.Bytes != 1 {
		t.Errorf("Stats after Close = %+v (held entry still charged)", got)
	}
	if b := readBody(t, held); string(b) != "x" {
		t.Errorf("held body after Close = %q", b)
	}
	held.Release()
	if got := s.Stats()[store.TierMem]; got.Bytes != 0 {
		t.Errorf("bytes after final Release = %d", got.Bytes)
	}
}

func TestConcurrent(t *testing.T) {
	t.Parallel()
	const (
		capacity = 1 << 20
		maxObj   = 64 << 10
		keyspace = 512
		writers  = 8
		readers  = 32
	)
	s, err := New(Config{Capacity: capacity, Shards: 16, MaxObjectBytes: maxObj})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	if testing.Short() {
		deadline = time.Now().Add(200 * time.Millisecond)
	}
	var (
		wg        sync.WaitGroup
		commits   atomic.Int64
		hits      atomic.Int64
		overshoot atomic.Int64
		corrupt   atomic.Int64
	)
	limit := int64(capacity) + s.Overdraft()
	check := func() {
		if b := s.total.Load(); b > limit {
			overshoot.Store(b)
		}
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 1))
			for time.Now().Before(deadline) {
				i := r.IntN(keyspace)
				size := 1 + r.IntN(maxObj)
				body := bytes.Repeat([]byte{byte(i)}, size)
				wr, err := s.Writer(ctx, key(i), store.Meta{Size: int64(size)})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := wr.Write(body[:size/2]); err != nil {
					t.Error(err)
					return
				}
				if _, err := wr.Write(body[size/2:]); err != nil {
					t.Error(err)
					return
				}
				e, err := wr.Commit(store.Meta{Size: int64(size)})
				if err != nil {
					if errors.Is(err, store.ErrTooLarge) {
						continue // every entry held; legitimate refusal
					}
					t.Error(err)
					return
				}
				commits.Add(1)
				check()
				e.Release()
			}
		}(uint64(w + 1))
	}
	for rd := 0; rd < readers; rd++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 2))
			for time.Now().Before(deadline) {
				i := r.IntN(keyspace)
				e, err := s.Get(ctx, key(i))
				if err != nil {
					continue
				}
				hits.Add(1)
				rc, _ := e.Open()
				b, _ := io.ReadAll(rc)
				_ = rc.Close()
				if int64(len(b)) != e.Meta.Size || (len(b) > 0 && (b[0] != byte(i) || b[len(b)-1] != byte(i))) {
					corrupt.Add(1)
				}
				check()
				if r.IntN(4) == 0 {
					time.Sleep(50 * time.Microsecond) // hold across writer churn
				}
				e.Release()
			}
		}(uint64(rd + 100))
	}
	wg.Wait()
	if o := overshoot.Load(); o != 0 {
		t.Errorf("bytes reached %d, over Capacity+Overdraft %d", o, limit)
	}
	if c := corrupt.Load(); c != 0 {
		t.Errorf("%d reads saw a wrong body", c)
	}
	if commits.Load() == 0 || hits.Load() == 0 {
		t.Errorf("no traffic: commits=%d hits=%d", commits.Load(), hits.Load())
	}
	audit(t, s)
	st := s.Stats()[store.TierMem]
	if st.Bytes > capacity+s.Overdraft() || st.Evictions == 0 {
		t.Errorf("final Stats = %+v", st)
	}
	t.Logf("commits=%d hits=%d evictions=%d bytes=%d entries=%d", commits.Load(), hits.Load(), st.Evictions, st.Bytes, st.Entries)
}

func BenchmarkGetHit(b *testing.B) {
	s, err := New(Config{Capacity: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	w, _ := s.Writer(ctx, key(1), store.Meta{})
	_, _ = w.Write(make([]byte, 4096))
	e, _ := w.Commit(store.Meta{URL: "u"})
	e.Release()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		e, err := s.Get(ctx, key(1))
		if err != nil {
			b.Fatal(err)
		}
		e.Release()
	}
}

func BenchmarkGetHitParallel(b *testing.B) {
	s, err := New(Config{Capacity: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	for i := range 64 {
		w, _ := s.Writer(ctx, key(i), store.Meta{})
		_, _ = w.Write(make([]byte, 1024))
		e, err := w.Commit(store.Meta{})
		if err != nil {
			b.Fatal(err)
		}
		e.Release()
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			e, err := s.Get(ctx, key(i&63))
			if err != nil {
				b.Fatal(err)
			}
			e.Release()
			i++
		}
	})
}

func BenchmarkWriterCommit(b *testing.B) {
	s, err := New(Config{Capacity: 64 << 20})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	body := make([]byte, 4096)
	m := store.Meta{Size: int64(len(body))}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	i := 0
	for b.Loop() {
		w, err := s.Writer(ctx, key(i&4095), m)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			b.Fatal(err)
		}
		e, err := w.Commit(m)
		if err != nil {
			b.Fatal(err)
		}
		e.Release()
		i++
	}
}
