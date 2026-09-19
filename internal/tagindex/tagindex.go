// Package tagindex maps surrogate keys (Cache-Tag / Surrogate-Key values) and
// URL prefixes to cache keys so a purge by tag or prefix can batch-delete. It is
// in-memory only and is rebuilt from store metas during disk recovery.
//
// STUB written by the integrator to freeze the contract; workstream C owns this
// package and replaces the bodies, keeping every exported signature.
package tagindex

import "github.com/crowbett/edgecache/internal/cachekey"

// Index is a concurrency-safe tag and URL index.
type Index struct{}

// NewIndex returns an empty index.
func NewIndex() *Index { return &Index{} }

// Add records k under every tag and under its canonical URL (for ByPrefix).
// Re-adding a key replaces its previous tags.
func (ix *Index) Add(k cachekey.Key, url string, tags []string) {
	_, _, _, _ = ix, k, url, tags
}

// Remove forgets k everywhere; unknown keys are a no-op.
func (ix *Index) Remove(k cachekey.Key) {
	_, _ = ix, k
}

// ByTag returns the keys recorded under tag, in no particular order.
func (ix *Index) ByTag(tag string) []cachekey.Key {
	_, _ = ix, tag
	return nil
}

// ByPrefix returns the keys whose canonical URL starts with prefix.
func (ix *Index) ByPrefix(prefix string) []cachekey.Key {
	_, _ = ix, prefix
	return nil
}

// Len reports how many keys are indexed (admin stats).
func (ix *Index) Len() int {
	_ = ix
	return 0
}
