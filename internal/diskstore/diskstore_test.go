package diskstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func newStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
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
	e, err := w.Commit(store.Meta{URL: "http://o/x", Method: "GET", ETag: `"e"`})
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

func countFiles(t *testing.T, dir, ext string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ext) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// audit recomputes the byte total from the index and tombstones.
func audit(t *testing.T, s *Store) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, fe := range s.idx {
		if fe.state != stateLive {
			t.Errorf("indexed entry in state %d", fe.state)
		}
		sum += fe.size
	}
	for _, fe := range s.tombs {
		if fe.state != stateEvicted {
			t.Errorf("tombstone in state %d", fe.state)
		}
		sum += fe.size
	}
	if sum != s.bytes {
		t.Errorf("bytes %d != indexed+tombstoned %d", s.bytes, sum)
	}
	if s.lru.Len() != len(s.idx) {
		t.Errorf("lru %d != index %d", s.lru.Len(), len(s.idx))
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"no dir", Config{Capacity: 1}, true},
		{"zero capacity", Config{Dir: t.TempDir()}, true},
		{"defaults", Config{Dir: t.TempDir(), Capacity: 1 << 20}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(tc.cfg, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%+v) err = %v, wantErr %v", tc.cfg, err, tc.wantErr)
			}
			if err == nil {
				if s.cfg.MaxObjectBytes != tc.cfg.Capacity || s.cfg.MaxVariants != DefaultMaxVariants {
					t.Errorf("defaults not applied: %+v", s.cfg)
				}
				_ = s.Close()
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStore(t, Config{Dir: dir, Capacity: 1 << 20})
	ctx := context.Background()
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Fatalf("Get empty = %v", err)
	}
	body := []byte("hello disk")
	e := put(t, s, key(1), body)
	if e.Tier != store.TierDisk || e.Meta.Size != int64(len(body)) || e.Meta.ETag != `"e"` {
		t.Errorf("entry = %+v", e)
	}
	if got := readBody(t, e); !bytes.Equal(got, body) {
		t.Errorf("body = %q", got)
	}
	e.Release()

	h := key(1).String()
	for _, p := range []string{filepath.Join(dir, h[:2], h+".body"), filepath.Join(dir, h[:2], h+".meta")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if n := countFiles(t, filepath.Join(dir, "tmp"), ""); n != 0 {
		t.Errorf("%d temp files left", n)
	}

	got, err := s.Get(ctx, key(1))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := got.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rc.(*os.File); !ok {
		t.Errorf("Open returned %T, want *os.File", rc)
	}
	_ = rc.Close()
	got.Release()

	if err := s.UpdateMeta(ctx, key(1), store.Meta{URL: "u2", ETag: `"f"`, Size: 999}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, key(1))
	if got.Meta.URL != "u2" || got.Meta.ETag != `"f"` || got.Meta.Size != int64(len(body)) {
		t.Errorf("after UpdateMeta = %+v", got.Meta)
	}
	got.Release()
	if err := s.UpdateMeta(ctx, key(2), store.Meta{}); !errors.Is(err, store.ErrMiss) {
		t.Errorf("UpdateMeta missing = %v", err)
	}

	// Overwrite replaces the pair; the old entry's readers see the swap.
	old, _ := s.Get(ctx, key(1))
	put(t, s, key(1), []byte("second")).Release()
	if _, err := old.Open(); !errors.Is(err, store.ErrMiss) {
		t.Errorf("Open on replaced entry = %v", err)
	}
	old.Release()
	got, _ = s.Get(ctx, key(1))
	if b := readBody(t, got); string(b) != "second" {
		t.Errorf("overwritten body = %q", b)
	}
	got.Release()
	if st := s.Stats()[store.TierDisk]; st.Entries != 1 || st.Bytes != 6 || st.Capacity != 1<<20 {
		t.Errorf("Stats = %+v", st)
	}

	// Rewrite refused while a reader still has the previous body open.
	held, _ := s.Get(ctx, key(1))
	rc, _ = held.Open()
	w, _ := s.Writer(ctx, key(1), store.Meta{})
	_, _ = w.Write([]byte("third"))
	committed, err := w.Commit(store.Meta{})
	_ = rc.Close()
	held.Release()
	switch {
	case err == nil:
		// Only NTFS refuses; elsewhere the rename goes through and Commit
		// hands back the new generation held - release it like any Get.
		committed.Release()
		got, _ = s.Get(ctx, key(1))
		if b := readBody(t, got); string(b) != "third" {
			t.Errorf("body after rewrite = %q", b)
		}
		got.Release()
	case errors.Is(err, ErrBusy):
		got, _ = s.Get(ctx, key(1))
		if b := readBody(t, got); string(b) != "second" {
			t.Errorf("body after refused rewrite = %q", b)
		}
		got.Release()
	default:
		t.Errorf("Commit over open body = %v, want ErrBusy", err)
	}
	if n := countFiles(t, filepath.Join(dir, "tmp"), ""); n != 0 {
		t.Errorf("%d temp files left after refused commit", n)
	}

	n, err := s.Delete(ctx, key(1), key(2))
	if err != nil || n != 1 {
		t.Errorf("Delete = %d, %v", n, err)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("Get after Delete = %v", err)
	}
	if st := s.Stats()[store.TierDisk]; st.Entries != 0 || st.Bytes != 0 {
		t.Errorf("Stats after Delete = %+v", st)
	}
	if n := countFiles(t, dir, ".body") + countFiles(t, dir, ".meta"); n != 0 {
		t.Errorf("%d files left after Delete", n)
	}
	c := s.Counters()
	if c.Hits == 0 || c.Misses != 2 {
		t.Errorf("Counters = %+v", c)
	}
	audit(t, s)
}

func TestCommitAbortAndAdmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStore(t, Config{Dir: dir, Capacity: 1 << 20, MaxObjectBytes: 100})
	ctx := context.Background()
	tmp := filepath.Join(dir, "tmp")

	w, _ := s.Writer(ctx, key(1), store.Meta{})
	_, _ = w.Write([]byte("abc"))
	if n := countFiles(t, tmp, ".body"); n != 1 {
		t.Errorf("temp files during write = %d", n)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("visible before Commit: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(store.Meta{}); !errors.Is(err, ErrCommitted) {
		t.Errorf("Commit after Abort = %v", err)
	}
	if n := countFiles(t, tmp, ""); n != 0 {
		t.Errorf("temp files after Abort = %d", n)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("after Abort Get = %v", err)
	}

	w, _ = s.Writer(ctx, key(2), store.Meta{})
	_, _ = w.Write([]byte("par"))
	_, _ = w.Write([]byte("tial"))
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
	e2, err := s.Get(ctx, key(2))
	if err != nil {
		t.Fatal(err)
	}
	if b := readBody(t, e2); string(b) != "partial" {
		t.Errorf("body = %q", b)
	}
	e2.Release()

	if _, err := s.Writer(ctx, key(3), store.Meta{Size: 101}); !errors.Is(err, store.ErrTooLarge) {
		t.Errorf("declared too large = %v", err)
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
	if n := countFiles(t, tmp, ""); n != 0 {
		t.Errorf("temp files after over-cap = %d", n)
	}
	audit(t, s)
}

func TestRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStore(t, Config{Dir: dir, Capacity: 1 << 20})
	ctx := context.Background()
	bodies := map[int][]byte{}
	for i := 1; i <= 5; i++ {
		bodies[i] = bytes.Repeat([]byte{byte(i)}, 10*i)
		w, _ := s.Writer(ctx, key(i), store.Meta{})
		_, _ = w.Write(bodies[i])
		m := store.Meta{URL: "http://o/p?i=" + string(rune('0'+i)), Method: "GET", Tags: []string{"t"}}
		m.Status = 200
		m.Header = map[string][]string{"Content-Type": {"text/plain"}}
		m.ResponseTime = time.Unix(int64(1000+i), 0).UTC()
		if i >= 4 {
			m.Vary = []string{"accept"}
			m.URL = "http://o/vary"
		}
		e, err := w.Commit(m)
		if err != nil {
			t.Fatal(err)
		}
		e.Release()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Hand-made damage: an orphan body, a corrupt meta, a meta whose size lies,
	// a meta naming another key, and a stale temp file.
	h := func(i int) string { return key(i).String() }
	sub := func(i int) string { return filepath.Join(dir, h(i)[:2]) }
	if err := os.WriteFile(filepath.Join(sub(9), h(9)+".body"), []byte("orphan"), 0o600); err != nil {
		_ = os.MkdirAll(sub(9), 0o750)
		if err := os.WriteFile(filepath.Join(sub(9), h(9)+".body"), []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sub(2), h(2)+".meta"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub(3), h(3)+".body"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	m4, err := os.ReadFile(filepath.Join(sub(4), h(4)+".meta"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub(5), h(5)+".meta"), m4, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "stale.body"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, Config{Dir: dir, Capacity: 1 << 20})
	c := s2.Counters()
	if c.Orphans != 1 || c.Corrupt != 3 {
		t.Errorf("recovery counters = %+v", c)
	}
	st := s2.Stats()[store.TierDisk]
	if st.Entries != 2 || st.Bytes != 10+40 {
		t.Errorf("Stats after recovery = %+v", st)
	}
	for _, i := range []int{1, 4} {
		e, err := s2.Get(ctx, key(i))
		if err != nil {
			t.Errorf("key %d not recovered: %v", i, err)
			continue
		}
		if e.Meta.Status != 200 || e.Meta.Header.Get("Content-Type") != "text/plain" || len(e.Meta.Tags) != 1 {
			t.Errorf("recovered meta %d = %+v", i, e.Meta)
		}
		if b := readBody(t, e); !bytes.Equal(b, bodies[i]) {
			t.Errorf("recovered body %d = %v", i, b[:min(4, len(b))])
		}
		e.Release()
	}
	for _, i := range []int{2, 3, 5, 9} {
		if _, err := s2.Get(ctx, key(i)); !errors.Is(err, store.ErrMiss) {
			t.Errorf("damaged key %d served: %v", i, err)
		}
	}
	if n := countFiles(t, dir, ".body") + countFiles(t, dir, ".meta"); n != 4 {
		t.Errorf("%d files on disk after recovery, want 4", n)
	}
	if n := countFiles(t, filepath.Join(dir, "tmp"), ""); n != 0 {
		t.Errorf("%d stale temp files left", n)
	}
	// Variants rebuilt from Vary metas under the primary key.
	vs, err := s2.Variants(ctx, cachekey.Primary("GET", mustURL(t, "http://o/vary")))
	if err != nil || len(vs) != 1 || vs[0].Key != key(4) || vs[0].Vary[0] != "accept" {
		t.Errorf("rebuilt variants = %v, %v", vs, err)
	}
	// Walk sees exactly the recovered set.
	seen := map[cachekey.Key]bool{}
	if err := s2.Walk(ctx, func(k cachekey.Key, m store.Meta) error {
		seen[k] = m.Tags[0] == "t"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[key(1)] || !seen[key(4)] {
		t.Errorf("walked %v", seen)
	}
	audit(t, s2)

	// A restart over budget trims the LRU tail; recovery orders by meta mtime,
	// so key 1 (written first, Get does not touch the file) goes before key 4.
	_ = s2.Close()
	s3 := newStore(t, Config{Dir: dir, Capacity: 45})
	if st := s3.Stats()[store.TierDisk]; st.Entries != 1 || st.Bytes != 40 || st.Evictions != 1 {
		t.Errorf("Stats after trimmed restart = %+v", st)
	}
	if _, err := s3.Get(ctx, key(4)); err != nil {
		t.Errorf("newest key trimmed: %v", err)
	}
	if n := countFiles(t, dir, ".body"); n != 1 {
		t.Errorf("%d bodies on disk after trimmed restart", n)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBudgetEviction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStore(t, Config{Dir: dir, Capacity: 300, MaxObjectBytes: 100})
	ctx := context.Background()
	body := make([]byte, 100)
	for i := 1; i <= 3; i++ {
		put(t, s, key(i), body).Release()
	}
	e, _ := s.Get(ctx, key(1)) // 1 is now most recent: LRU order 2,3,1
	e.Release()
	put(t, s, key(4), body).Release()
	if _, err := s.Get(ctx, key(2)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("key 2 should be evicted: %v", err)
	}
	for _, i := range []int{1, 3, 4} {
		e, err := s.Get(ctx, key(i))
		if err != nil {
			t.Errorf("key %d evicted unexpectedly", i)
			continue
		}
		e.Release()
	}
	st := s.Stats()[store.TierDisk]
	if st.Bytes != 300 || st.Entries != 3 || st.Evictions != 1 {
		t.Errorf("Stats = %+v", st)
	}
	if n := countFiles(t, dir, ".body"); n != 3 {
		t.Errorf("%d bodies on disk", n)
	}
	if s.Tombstones() != 0 {
		t.Errorf("tombstones = %d", s.Tombstones())
	}
	audit(t, s)
}

func TestEvictWhileReadingTombstone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStore(t, Config{Dir: dir, Capacity: 300, MaxObjectBytes: 100})
	ctx := context.Background()
	body := bytes.Repeat([]byte{7}, 100)
	for i := 1; i <= 3; i++ {
		put(t, s, key(i), body).Release()
	}
	reader, err := s.Get(ctx, key(1)) // LRU: 2,3,1 - but 1 will be tail after 2,3 are touched
	if err != nil {
		t.Fatal(err)
	}
	rc, err := reader.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{2, 3} {
		e, _ := s.Get(ctx, key(i))
		e.Release()
	}
	// Key 1 is the LRU tail and held: eviction tombstones it, which frees
	// nothing, so the commit takes a second victim (2) to fit. The tombstone
	// stays counted as resident: 100 (tomb) + 3 + 4.
	put(t, s, key(4), body).Release()
	for _, i := range []int{1, 2} {
		if _, err := s.Get(ctx, key(i)); !errors.Is(err, store.ErrMiss) {
			t.Errorf("evicted key %d still visible: %v", i, err)
		}
	}
	if s.Tombstones() != 1 {
		t.Errorf("tombstones = %d, want 1", s.Tombstones())
	}
	if st := s.Stats()[store.TierDisk]; st.Bytes != 300 || st.Entries != 2 || st.Evictions != 2 {
		t.Errorf("Stats with tombstone = %+v (tombstone counts as resident)", st)
	}
	// The reader keeps streaming the tombstoned body.
	b, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(b, body) {
		t.Errorf("read after evict = %d bytes, %v", len(b), err)
	}
	// Sweep cannot free a held tombstone.
	if n, err := s.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("Sweep while held = %d, %v", n, err)
	}
	if s.Tombstones() != 1 {
		t.Errorf("tombstone freed while held")
	}
	// Release with the file still open: NTFS refuses the unlink, Sweep after Close clears it.
	reader.Release()
	_ = rc.Close()
	if n, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 && n != 1 {
		t.Errorf("Sweep freed %d", n)
	}
	if s.Tombstones() != 0 {
		t.Errorf("tombstones after Sweep = %d", s.Tombstones())
	}
	if st := s.Stats()[store.TierDisk]; st.Bytes != 200 || st.Entries != 2 {
		t.Errorf("Stats after Sweep = %+v", st)
	}
	if n := countFiles(t, dir, ".body"); n != 2 {
		t.Errorf("%d bodies on disk after Sweep", n)
	}
	audit(t, s)

	// Delete of a held entry tombstones it too, and a rewrite under that key
	// while the tombstone is unlinkable is refused rather than clobbered.
	held, _ := s.Get(ctx, key(4))
	rc, _ = held.Open()
	if n, _ := s.Delete(ctx, key(4)); n != 1 {
		t.Fatal("Delete held")
	}
	if s.Tombstones() != 1 {
		t.Errorf("tombstones after Delete held = %d", s.Tombstones())
	}
	if b := readBodyFrom(t, rc); !bytes.Equal(b, body) {
		t.Errorf("held body after Delete = %d bytes", len(b))
	}
	held.Release()
	_ = rc.Close()
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Tombstones() != 0 {
		t.Errorf("tombstone survived Sweep after release+close")
	}
	audit(t, s)
}

func readBodyFrom(t *testing.T, rc io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVariantCap(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{Capacity: 1 << 20, MaxVariants: 2})
	ctx := context.Background()
	primary := key(100)
	for i := 1; i <= 3; i++ {
		put(t, s, key(i), []byte("v")).Release()
		v := store.Variant{Vary: []string{"accept"}, Key: key(i), Seen: time.Unix(int64(i), 0)}
		if err := s.PutVariant(ctx, primary, v); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.Variants(ctx, primary)
	if err != nil || len(vs) != 2 || vs[0].Key != key(3) || vs[1].Key != key(2) {
		t.Errorf("variants = %v, %v", vs, err)
	}
	if _, err := s.Get(ctx, key(1)); !errors.Is(err, store.ErrMiss) {
		t.Errorf("oldest variant's entry not evicted: %v", err)
	}
	if c := s.Counters(); c.VariantEvictions != 1 || c.Evictions != 1 {
		t.Errorf("Counters = %+v", c)
	}
	if _, err := s.Delete(ctx, primary); err != nil {
		t.Fatal(err)
	}
	if vs, _ := s.Variants(ctx, primary); len(vs) != 0 {
		t.Errorf("variants after Delete(primary) = %v", vs)
	}
}

func TestClose(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{Capacity: 1 << 20})
	ctx := context.Background()
	put(t, s, key(1), []byte("x")).Release()
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
	if _, err := s.Sweep(ctx); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Sweep after Close = %v", err)
	}
	if b := readBody(t, held); string(b) != "x" {
		t.Errorf("held body after Close = %q", b)
	}
	held.Release()
}

func TestConcurrent(t *testing.T) {
	t.Parallel()
	const (
		capacity = 512 << 10
		maxObj   = 16 << 10
		keyspace = 64
		writers  = 4
		readers  = 16
	)
	s := newStore(t, Config{Capacity: capacity, MaxObjectBytes: maxObj})
	ctx := context.Background()
	deadline := time.Now().Add(1500 * time.Millisecond)
	if testing.Short() {
		deadline = time.Now().Add(200 * time.Millisecond)
	}
	var (
		wg                        sync.WaitGroup
		commits, hits, busy       atomic.Int64
		overshoot, corrupt, other atomic.Int64
	)
	// A slow runner (windows-latest under -race) can spend the whole window on
	// a handful of fsynced commits before any reader finds one: keep going past
	// the deadline until both sides saw traffic, bounded by a hard stop.
	hard := time.Now().Add(15 * time.Second)
	running := func() bool {
		now := time.Now()
		return now.Before(deadline) || ((commits.Load() == 0 || hits.Load() == 0) && now.Before(hard))
	}
	limit := int64(capacity) + s.Overdraft()
	check := func() {
		if b := s.Stats()[store.TierDisk].Bytes; b > limit {
			overshoot.Store(b)
		}
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 1))
			for running() {
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
					switch {
					case errors.Is(err, ErrBusy):
						busy.Add(1)
					case errors.Is(err, store.ErrTooLarge):
					default:
						other.Add(1)
						t.Logf("commit: %v", err)
					}
					continue
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
			for running() {
				i := r.IntN(keyspace)
				e, err := s.Get(ctx, key(i))
				if err != nil {
					continue
				}
				hits.Add(1)
				rc, err := e.Open()
				if err != nil {
					// Replaced under us between Get and Open: legitimate.
					if !errors.Is(err, store.ErrMiss) {
						other.Add(1)
						t.Logf("open: %v", err)
					}
					e.Release()
					continue
				}
				b, err := io.ReadAll(rc)
				if err != nil || int64(len(b)) != e.Meta.Size || (len(b) > 0 && (b[0] != byte(i) || b[len(b)-1] != byte(i))) {
					corrupt.Add(1)
				}
				if r.IntN(4) == 0 {
					time.Sleep(50 * time.Microsecond)
				}
				_ = rc.Close()
				e.Release()
				check()
			}
		}(uint64(rd + 100))
	}
	wg.Wait()
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if o := overshoot.Load(); o != 0 {
		t.Errorf("bytes reached %d, over Capacity+Overdraft %d", o, limit)
	}
	if c := corrupt.Load(); c != 0 {
		t.Errorf("%d reads saw a wrong body", c)
	}
	if o := other.Load(); o != 0 {
		t.Errorf("%d unexpected errors", o)
	}
	if commits.Load() == 0 || hits.Load() == 0 {
		t.Errorf("no traffic: commits=%d hits=%d", commits.Load(), hits.Load())
	}
	audit(t, s)
	st := s.Stats()[store.TierDisk]
	if st.Bytes > limit {
		t.Errorf("final Stats = %+v", st)
	}
	// Every indexed entry has its pair on disk and nothing else is left over.
	if n := countFiles(t, s.cfg.Dir, ".body"); int64(n) != st.Entries+int64(s.Tombstones()) {
		t.Errorf("%d bodies on disk, %d entries + %d tombstones", n, st.Entries, s.Tombstones())
	}
	t.Logf("commits=%d busy=%d hits=%d evictions=%d bytes=%d entries=%d tombstones=%d",
		commits.Load(), busy.Load(), hits.Load(), st.Evictions, st.Bytes, st.Entries, s.Tombstones())
}
