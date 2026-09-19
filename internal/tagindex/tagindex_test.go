package tagindex

import (
	"crypto/sha256"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/crowbett/edgecache/internal/cachekey"
)

func key(s string) cachekey.Key { return cachekey.Key(sha256.Sum256([]byte(s))) }

func sorted(ks []cachekey.Key) []string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.String())
	}
	slices.Sort(out)
	return out
}

func TestAddByTagByPrefixRemove(t *testing.T) {
	t.Parallel()
	ix := NewIndex()
	k1, k2, k3 := key("1"), key("2"), key("3")
	ix.Add(k1, "http://a/x/1", []string{"news", " sports "})
	ix.Add(k2, "http://a/x/2", []string{"news"})
	ix.Add(k3, "http://a/y/3", []string{"", "sports"})

	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
	if got, want := sorted(ix.ByTag("news")), sorted([]cachekey.Key{k1, k2}); !slices.Equal(got, want) {
		t.Fatalf("ByTag(news) = %v, want %v", got, want)
	}
	if got, want := sorted(ix.ByTag(" sports ")), sorted([]cachekey.Key{k1, k3}); !slices.Equal(got, want) {
		t.Fatalf("ByTag(sports) = %v, want %v", got, want)
	}
	if got := ix.ByTag("missing"); got != nil {
		t.Fatalf("ByTag(missing) = %v, want nil", got)
	}
	if got, want := sorted(ix.ByPrefix("http://a/x/")), sorted([]cachekey.Key{k1, k2}); !slices.Equal(got, want) {
		t.Fatalf("ByPrefix = %v, want %v", got, want)
	}
	if got := ix.ByPrefix("http://a/"); len(got) != 3 {
		t.Fatalf("ByPrefix(all) = %d keys, want 3", len(got))
	}
	if u, ok := ix.URL(k3); !ok || u != "http://a/y/3" {
		t.Fatalf("URL(k3) = %q,%v", u, ok)
	}

	// Re-adding replaces the tags.
	ix.Add(k1, "http://a/x/1", []string{"other"})
	if got := ix.ByTag("news"); len(got) != 1 || got[0] != k2 {
		t.Fatalf("after re-add ByTag(news) = %v, want [k2]", got)
	}
	if got := ix.ByTag("other"); len(got) != 1 || got[0] != k1 {
		t.Fatalf("ByTag(other) = %v, want [k1]", got)
	}

	ix.Remove(k2)
	ix.Remove(k2) // unknown is a no-op
	if ix.Len() != 2 || ix.ByTag("news") != nil {
		t.Fatalf("after Remove: Len=%d news=%v", ix.Len(), ix.ByTag("news"))
	}
	if _, ok := ix.URL(k2); ok {
		t.Fatal("URL(k2) still present after Remove")
	}
	ix.Remove(k1)
	ix.Remove(k3)
	if ix.Len() != 0 || len(ix.tags) != 0 {
		t.Fatalf("index not empty: keys=%d tags=%d", ix.Len(), len(ix.tags))
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	ix := NewIndex()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := key(strconv.Itoa(i))
			for range 100 {
				ix.Add(k, "http://h/p/"+strconv.Itoa(i%4), []string{"t" + strconv.Itoa(i%3)})
				_ = ix.ByTag("t0")
				_ = ix.ByPrefix("http://h/p/1")
				_ = ix.Len()
			}
			if i%2 == 0 {
				ix.Remove(k)
			}
		}()
	}
	wg.Wait()
	if ix.Len() != 32 {
		t.Fatalf("Len = %d, want 32", ix.Len())
	}
}
