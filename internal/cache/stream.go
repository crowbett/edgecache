package cache

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/crowbett/edgecache/internal/store"
)

// errStreamAbandoned ends a stream whose leader stopped reading the origin
// body because nobody needed the rest (client gone, store writer aborted, no
// readers attached).
var errStreamAbandoned = errors.New("cache: stream abandoned by its leader")

// stream fans a collapse leader's origin body out to the waiters that joined
// it, while the body is still downloading: a waiter on a cold miss gets its
// first byte when the leader does instead of after the leader's commit.
//
// It retains the body from byte 0 while it is open to new readers, so a late
// joiner can start from the beginning; once the body passes max bytes it
// closes to new readers (they fall back to waiting for the commit) and keeps
// only the chunks some attached reader has not yet consumed. Memory per
// in-flight miss is therefore at most max while open, and afterwards the gap
// between the leader and its slowest attached reader. Chunks are immutable
// once appended, so readers write them to their clients without copying.
type stream struct {
	meta   store.Meta // the entry being stored: what waiters serve
	length int64      // origin Content-Length, -1 when unknown
	max    int64

	mu      sync.Mutex
	chunks  [][]byte
	first   int   // absolute index of chunks[0]
	size    int64 // bytes appended so far
	closed  bool  // no new readers: over max, abandoned or ended
	ended   bool  // body complete (err == nil) or failed
	err     error
	readers map[*streamReader]struct{} // allocated by the first attach
	wake    chan struct{}              // closed and replaced on append / end while a reader is attached

	settled chan struct{} // closed by settle once the leader's fetch result is final
	final   fetchResult
}

func newStream(meta store.Meta, length, maxBytes int64) *stream {
	return &stream{
		meta:    meta,
		length:  length,
		max:     maxBytes,
		wake:    make(chan struct{}),
		settled: make(chan struct{}),
	}
}

// streamReader is one waiter's cursor: the absolute index of the next chunk.
type streamReader struct {
	s   *stream
	idx int
}

// attach adds a reader positioned at byte 0; ok is false once the stream is
// closed to new readers.
func (s *stream) attach() (*streamReader, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.first != 0 {
		return nil, false
	}
	r := &streamReader{s: s}
	if s.readers == nil {
		s.readers = make(map[*streamReader]struct{})
	}
	s.readers[r] = struct{}{}
	return r, true
}

// append publishes the next chunk of the body (copied: p is the leader's
// reusable buffer). A nil stream ignores it.
func (s *stream) append(p []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.size += int64(len(p))
	if s.closed && len(s.readers) == 0 {
		return // nobody can ever read it
	}
	s.chunks = append(s.chunks, append([]byte(nil), p...))
	if !s.closed && s.size > s.max {
		s.closed = true
	}
	s.trimLocked()
	s.wakeLocked()
}

// abandon reports whether the leader may stop reading the origin body because
// no reader depends on it, and if so closes the stream to new readers in the
// same step (a reader cannot attach between the check and the close). A nil
// stream is always abandonable.
func (s *stream) abandon() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.readers) > 0 {
		return false
	}
	s.closed = true
	s.trimLocked()
	return true
}

// end marks the body complete (err nil) or failed; readers drain what is
// retained and then see io.EOF or err. Later calls are ignored. A nil stream
// ignores it.
func (s *stream) end(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended, s.closed, s.err = true, true, err
	s.trimLocked()
	s.wakeLocked()
}

// settle records the leader's final fetch result for waiters that could not
// read the stream and fall back to the committed entry. It must be called
// exactly once, after end. A nil stream ignores it.
func (s *stream) settle(res fetchResult) {
	if s == nil {
		return
	}
	s.final = res
	close(s.settled)
}

// wait blocks until the leader's fetch result is final or ctx ends.
func (s *stream) wait(ctx context.Context) (fetchResult, error) {
	select {
	case <-s.settled:
		return s.final, nil
	case <-ctx.Done():
		return fetchResult{}, ctx.Err()
	}
}

// next returns the reader's next chunk, blocking until the leader appends one;
// io.EOF after the last chunk of a complete body, the leader's error after a
// failed one, ctx's error if ctx ends first. The chunk must not be modified.
func (r *streamReader) next(ctx context.Context) ([]byte, error) {
	s := r.s
	for {
		s.mu.Lock()
		if i := r.idx - s.first; i < len(s.chunks) {
			p := s.chunks[i]
			r.idx++
			s.trimLocked()
			s.mu.Unlock()
			return p, nil
		}
		if s.ended {
			err := s.err
			s.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return nil, err
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// close detaches the reader; chunks only it still needed are released.
func (r *streamReader) close() {
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.readers, r)
	s.trimLocked()
}

// trimLocked drops the leading chunks every attached reader has consumed. It
// does nothing while the stream is open to new readers: they start at byte 0.
func (s *stream) trimLocked() {
	if !s.closed {
		return
	}
	low := s.first + len(s.chunks)
	for r := range s.readers {
		low = min(low, r.idx)
	}
	n := low - s.first
	if n <= 0 {
		return
	}
	clear(s.chunks[:n])
	s.chunks = s.chunks[n:]
	s.first = low
}

// wakeLocked releases readers blocked in next. Only an attached reader can be
// blocked on s.wake, so with none there is nothing to close.
func (s *stream) wakeLocked() {
	if len(s.readers) == 0 {
		return
	}
	close(s.wake)
	s.wake = make(chan struct{})
}
