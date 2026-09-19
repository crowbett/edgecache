// Package store holds the port types shared by every storage tier: Meta, Entry,
// Variant, Writer, Stats and the sentinel errors. It is types only, no I/O; the
// Store interface itself lives in the consumer (internal/cache).
//
// STUB written by the integrator to freeze the contract; workstream B owns this
// package and replaces the bodies, keeping every exported signature. Meta and
// Writer are frozen at step 1 and change only through the integrator.
package store

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/rfc9111"
)

// Sentinel errors every tier returns; callers match with errors.Is.
var (
	// ErrMiss means no entry is stored under the key.
	ErrMiss = errors.New("store: miss")
	// ErrTooLarge means the object exceeds the tier's admission cap; the writer is aborted.
	ErrTooLarge = errors.New("store: too large")
	// ErrClosed means the store has been closed.
	ErrClosed = errors.New("store: closed")
)

// Tier names used in Entry.Tier and the Stats map.
const (
	TierMem  = "mem"
	TierDisk = "disk"
)

// Meta is everything the store persists about an entry besides its body. Vary is
// the normalised list (empty = the no-vary variant); Tags are the surrogate keys
// read from the configured tag headers; Size is the body length in bytes.
type Meta struct {
	rfc9111.Resp
	Method, URL, ETag string
	Vary, Tags        []string
	Size              int64
	LastModified      time.Time
}

// Variant records one Vary-selected secondary key under a primary key, with the
// time it was last seen for LRU capping.
type Variant struct {
	Vary []string
	Key  cachekey.Key
	Seen time.Time
}

// Entry is a held reference to a stored object. Get returns it with one
// reference taken; Release drops it and is mandatory. Tier is "mem" or "disk".
type Entry struct {
	Key  cachekey.Key
	Meta Meta
	Tier string

	open    func() (io.ReadCloser, error)
	release func()
	once    sync.Once
}

// NewEntry builds an Entry whose Open and Release delegate to the tier's
// callbacks; either may be nil (Open then fails with ErrClosed, Release is a
// no-op). Tiers construct entries only through this.
func NewEntry(k cachekey.Key, m Meta, tier string, open func() (io.ReadCloser, error), release func()) *Entry {
	return &Entry{Key: k, Meta: m, Tier: tier, open: open, release: release}
}

// Open returns a fresh reader over the body: a *bytes.Reader for the memory
// tier, an *os.File for disk. Each call is independent; close what you open.
func (e *Entry) Open() (io.ReadCloser, error) {
	if e.open == nil {
		return nil, ErrClosed
	}
	return e.open()
}

// Release drops the reference Get took. It is idempotent.
func (e *Entry) Release() {
	e.once.Do(func() {
		if e.release != nil {
			e.release()
		}
	})
}

// Writer streams one body into a tier. Commit publishes atomically (temp +
// rename on disk) and returns a held entry; Abort discards, is idempotent and
// safe in a defer after Commit.
type Writer interface {
	io.Writer
	Commit(final Meta) (*Entry, error)
	Abort() error
}

// Stats is one tier's gauge snapshot.
type Stats struct {
	Bytes, Entries, Evictions, Capacity int64
}
