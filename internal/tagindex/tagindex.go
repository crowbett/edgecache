// Package tagindex maps surrogate keys (Cache-Tag / Surrogate-Key values) and
// URL prefixes to cache keys so a purge by tag or prefix can batch-delete. It is
// in-memory only and is rebuilt from store metas during disk recovery.
package tagindex

import (
	"strings"
	"sync"

	"github.com/crowbett/edgecache/internal/cachekey"
)

// record is what the index remembers about one key; gen is the Add that
// created the row, so a reconcile can tell a row it snapshotted from one
// re-added since.
type record struct {
	url  string
	tags []string
	gen  uint64
}

// Index is a concurrency-safe tag and URL index.
type Index struct {
	mu   sync.RWMutex
	gen  uint64
	keys map[cachekey.Key]record
	tags map[string]map[cachekey.Key]struct{}
}

// Stamp names one row at the time of a Snapshot; RemoveStale skips a key whose
// row was re-added since.
type Stamp struct {
	Key cachekey.Key
	gen uint64
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		keys: make(map[cachekey.Key]record),
		tags: make(map[string]map[cachekey.Key]struct{}),
	}
}

// Add records k under every tag and under its canonical URL (for ByPrefix).
// Re-adding a key replaces its previous tags. Tags are trimmed; empty ones are
// dropped.
func (ix *Index) Add(k cachekey.Key, url string, tags []string) {
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			clean = append(clean, t)
		}
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(k)
	ix.gen++
	ix.keys[k] = record{url: url, tags: clean, gen: ix.gen}
	for _, t := range clean {
		set := ix.tags[t]
		if set == nil {
			set = make(map[cachekey.Key]struct{})
			ix.tags[t] = set
		}
		set[k] = struct{}{}
	}
}

// Remove forgets k everywhere; unknown keys are a no-op.
func (ix *Index) Remove(k cachekey.Key) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(k)
}

func (ix *Index) removeLocked(k cachekey.Key) {
	rec, ok := ix.keys[k]
	if !ok {
		return
	}
	delete(ix.keys, k)
	for _, t := range rec.tags {
		set := ix.tags[t]
		delete(set, k)
		if len(set) == 0 {
			delete(ix.tags, t)
		}
	}
}

// ByTag returns the keys recorded under tag, in no particular order.
func (ix *Index) ByTag(tag string) []cachekey.Key {
	tag = strings.TrimSpace(tag)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	set := ix.tags[tag]
	if len(set) == 0 {
		return nil
	}
	out := make([]cachekey.Key, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// ByPrefix returns the keys whose canonical URL starts with prefix. It is a
// linear scan over the indexed keys.
func (ix *Index) ByPrefix(prefix string) []cachekey.Key {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []cachekey.Key
	for k, rec := range ix.keys {
		if strings.HasPrefix(rec.url, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// Snapshot returns a stamp for every indexed row. Pair it with RemoveStale to
// drop rows whose entries the store has since evicted: snapshot first, walk
// the store, then RemoveStale with the walk's result, so a row added during
// the walk is never touched.
func (ix *Index) Snapshot() []Stamp {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]Stamp, 0, len(ix.keys))
	for k, rec := range ix.keys {
		out = append(out, Stamp{Key: k, gen: rec.gen})
	}
	return out
}

// RemoveStale removes every snapshotted row that alive reports gone and that
// no Add has replaced since the snapshot; it returns how many went. alive is
// called without the index lock held.
func (ix *Index) RemoveStale(snap []Stamp, alive func(cachekey.Key) bool) int {
	var dead []Stamp
	for _, st := range snap {
		if !alive(st.Key) {
			dead = append(dead, st)
		}
	}
	if len(dead) == 0 {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	n := 0
	for _, st := range dead {
		if rec, ok := ix.keys[st.Key]; ok && rec.gen == st.gen {
			ix.removeLocked(st.Key)
			n++
		}
	}
	return n
}

// URL returns the canonical URL recorded for k and whether k is indexed.
func (ix *Index) URL(k cachekey.Key) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	rec, ok := ix.keys[k]
	return rec.url, ok
}

// Len reports how many keys are indexed (admin stats).
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.keys)
}
