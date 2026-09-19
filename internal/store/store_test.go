package store

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crowbett/edgecache/internal/cachekey"
)

func TestEntryOpenRelease(t *testing.T) {
	t.Parallel()
	released := 0
	e := NewEntry(cachekey.Key{1}, Meta{Size: 5}, TierMem,
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("hello")), nil },
		func() { released++ })
	rc, err := e.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello" || e.Tier != TierMem || e.Meta.Size != 5 {
		t.Errorf("entry = %q / %s / %d", b, e.Tier, e.Meta.Size)
	}
	e.Release()
	e.Release()
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}

	nilEntry := NewEntry(cachekey.Key{}, Meta{}, TierDisk, nil, nil)
	if _, err := nilEntry.Open(); !errors.Is(err, ErrClosed) {
		t.Errorf("Open on nil opener = %v, want ErrClosed", err)
	}
	nilEntry.Release() // must not panic
}
