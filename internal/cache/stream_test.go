package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/store"
)

// readAll drains r until EOF or an error and returns the bytes and the error
// that ended it (nil on EOF).
func readAll(ctx context.Context, r *streamReader) ([]byte, error) {
	var out []byte
	for {
		p, err := r.next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p...)
	}
}

func TestStreamDeliversInOrderThenEOF(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 1<<20)
	r, ok := s.attach()
	if !ok {
		t.Fatal("attach refused on a fresh stream")
	}
	defer r.close()
	buf := []byte("aaaa")
	s.append(buf)
	copy(buf, "bbbb") // the leader reuses its buffer: the stream must have copied
	s.append(buf)
	s.end(nil)
	got, err := readAll(context.Background(), r)
	if err != nil || string(got) != "aaaabbbb" {
		t.Fatalf("got %q, %v; want aaaabbbb, nil", got, err)
	}
}

func TestStreamLateReaderStartsAtZero(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 1<<20)
	s.append([]byte("one,"))
	s.append([]byte("two,"))
	r, ok := s.attach()
	if !ok {
		t.Fatal("attach refused under budget")
	}
	defer r.close()
	s.append([]byte("three"))
	s.end(nil)
	if got, _ := readAll(context.Background(), r); string(got) != "one,two,three" {
		t.Fatalf("got %q", got)
	}
}

func TestStreamOverBudgetClosesAndTrims(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 10)
	r, _ := s.attach()
	defer r.close()
	s.append(bytes.Repeat([]byte("a"), 8))
	r2, ok := s.attach()
	if !ok {
		t.Fatal("attach refused at 8/10 bytes")
	}
	r2.close() // only r pins chunks from here on

	s.append(bytes.Repeat([]byte("b"), 8)) // 16 > 10: closed to new readers
	if _, ok := s.attach(); ok {
		t.Fatal("attach accepted past the budget")
	}
	if _, err := r.next(context.Background()); err != nil { // consumes chunk 0
		t.Fatal(err)
	}
	s.mu.Lock()
	kept, first := len(s.chunks), s.first
	s.mu.Unlock()
	if kept != 1 || first != 1 {
		t.Fatalf("after consuming chunk 0: kept %d chunks from %d, want 1 from 1", kept, first)
	}
	s.append([]byte("c"))
	s.end(nil)
	if got, _ := readAll(context.Background(), r); string(got) != "bbbbbbbbc" {
		t.Fatalf("rest = %q", got)
	}
	s.mu.Lock()
	kept = len(s.chunks)
	s.mu.Unlock()
	if kept != 0 {
		t.Fatalf("%d chunks retained after the only reader drained", kept)
	}
}

func TestStreamClosedWithoutReadersRetainsNothing(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 4)
	s.append([]byte("12345")) // over budget, nobody attached
	s.append([]byte("6789"))
	s.mu.Lock()
	kept := len(s.chunks)
	s.mu.Unlock()
	if kept != 0 {
		t.Fatalf("%d chunks retained with no possible reader", kept)
	}
}

func TestStreamAbandon(t *testing.T) {
	t.Parallel()
	var nilStream *stream
	if !nilStream.abandon() {
		t.Error("nil stream must be abandonable")
	}
	s := newStream(store.Meta{}, -1, 1<<20)
	r, _ := s.attach()
	if s.abandon() {
		t.Fatal("abandoned with a reader attached")
	}
	r.close()
	if !s.abandon() {
		t.Fatal("not abandonable after the last reader left")
	}
	if _, ok := s.attach(); ok {
		t.Fatal("attach accepted after abandon")
	}
}

func TestStreamErrorAfterRetainedChunks(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 1<<20)
	r, _ := s.attach()
	defer r.close()
	s.append([]byte("partial"))
	boom := errors.New("origin reset")
	s.end(boom)
	s.end(nil) // ignored: the first end wins
	got, err := readAll(context.Background(), r)
	if string(got) != "partial" || !errors.Is(err, boom) {
		t.Fatalf("got %q, %v; want partial, %v", got, err, boom)
	}
}

func TestStreamNextHonoursContext(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 1<<20)
	r, _ := s.attach()
	defer r.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("next on an idle stream = %v, want deadline exceeded", err)
	}
}

func TestStreamSettle(t *testing.T) {
	t.Parallel()
	s := newStream(store.Meta{}, -1, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.wait(ctx); err == nil {
		t.Fatal("wait returned before settle")
	}
	s.end(nil)
	s.settle(fetchResult{stored: true, status: 200})
	res, err := s.wait(context.Background())
	if err != nil || !res.stored || res.status != 200 {
		t.Fatalf("wait = %+v, %v", res, err)
	}
	var nilStream *stream
	nilStream.append([]byte("x"))
	nilStream.end(nil)
	nilStream.settle(fetchResult{})
}

func TestStreamConcurrentReaders(t *testing.T) {
	t.Parallel()
	const (
		readers = 16
		chunks  = 500
	)
	s := newStream(store.Meta{}, -1, 1<<10) // closes early: readers pin and trim
	var want bytes.Buffer
	for i := range chunks {
		fmt.Fprintf(&want, "%04d|", i)
	}
	var (
		wg  sync.WaitGroup
		got [readers][]byte
	)
	rs := make([]*streamReader, readers)
	for i := range rs {
		var ok bool
		if rs[i], ok = s.attach(); !ok {
			t.Fatal("attach refused")
		}
	}
	for i, r := range rs {
		wg.Go(func() {
			defer r.close()
			b, err := readAll(context.Background(), r)
			if err != nil {
				t.Errorf("reader %d: %v", i, err)
			}
			got[i] = b
		})
	}
	buf := make([]byte, 5)
	for i := range chunks {
		copy(buf, fmt.Sprintf("%04d|", i))
		s.append(buf)
	}
	s.end(nil)
	wg.Wait()
	for i := range got {
		if !bytes.Equal(got[i], want.Bytes()) {
			t.Fatalf("reader %d got %d bytes, want %d", i, len(got[i]), want.Len())
		}
	}
	s.mu.Lock()
	kept := len(s.chunks)
	s.mu.Unlock()
	if kept != 0 {
		t.Fatalf("%d chunks retained after every reader drained", kept)
	}
}
