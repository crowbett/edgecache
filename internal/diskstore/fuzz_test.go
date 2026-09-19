package diskstore

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/store"
)

// FuzzDecodeMeta is the on-disk meta parser: arbitrary bytes never panic, an
// accepted meta always satisfies the invariants loadPair relies on (size,
// status, key), the byte cap holds, and a meta the store wrote itself round
// trips exactly.
func FuzzDecodeMeta(f *testing.F) {
	k := key(1)
	good := store.Meta{
		Method: http.MethodGet, URL: "http://o/x?a=1", ETag: `"e"`, Vary: []string{"accept"}, Tags: []string{"t1", "t2"},
		Size: 42, LastModified: time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC),
	}
	good.Status = 200
	good.Header = http.Header{"Content-Type": {"text/plain"}, "Cache-Control": {"max-age=60"}}
	good.RequestTime = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	good.ResponseTime = good.RequestTime.Add(50 * time.Millisecond)
	good.Lifetime = 60 * time.Second
	enc, err := encodeMeta(k, good)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(enc, k.String())
	f.Add(enc, key(2).String())
	f.Add([]byte("{not json"), k.String())
	f.Add([]byte(`{"v":2,"key":"`+k.String()+`","meta":{}}`), k.String())
	f.Add([]byte(`{"v":1,"key":"`+k.String()+`","meta":{"Size":-1,"Status":200}}`), k.String())
	f.Add([]byte(`{"v":1,"key":"`+k.String()+`","meta":{"Size":1,"Status":0}}`), k.String())
	f.Add([]byte(`{"v":1,"key":"`+k.String()+`","meta":{"Size":1,"Status":200},"extra":1}`), k.String())
	f.Add([]byte(`{"v":1,"key":"`+k.String()+`","meta":{"Size":1,"Status":200}} trailing`), k.String())
	f.Add([]byte(`{"v":1,"key":"`+k.String()+`","meta":{"Size":1,"Status":200,"Header":{"A":["b"]}}}`), k.String())
	f.Add([]byte("null"), k.String())
	f.Add([]byte(""), k.String())
	f.Add([]byte("[]"), k.String())
	f.Add([]byte(strings.Repeat(" ", 100)), k.String())
	f.Fuzz(func(t *testing.T, b []byte, base string) {
		m, err := decodeMeta(b, base)
		if err != nil {
			if !reflect.DeepEqual(m, store.Meta{}) {
				t.Fatal("meta returned alongside an error")
			}
			return
		}
		if m.Size < 0 {
			t.Fatalf("negative size %d accepted", m.Size)
		}
		if m.Status < 100 || m.Status > 999 {
			t.Fatalf("status %d accepted", m.Status)
		}
		if len(b) > maxMetaBytes {
			t.Fatal("over-cap meta accepted")
		}
		// Whatever was accepted must survive a re-encode under its own key.
		kk, ok := keyFromHex(base)
		if !ok {
			t.Fatalf("accepted a meta for a base that is not a key: %q", base)
		}
		again, err := encodeMeta(kk, m)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		m2, err := decodeMeta(again, base)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !metaEqual(m, m2) {
			t.Fatalf("round trip differs:\n%+v\n%+v", m, m2)
		}
		if len(again) > len(b)+1024 { // every field is emitted, zero or not: fixed overhead only
			t.Fatalf("re-encoded meta grew from %d to %d bytes", len(b), len(again))
		}
	})
}

func TestEncodeDecodeMetaRoundTrip(t *testing.T) {
	t.Parallel()
	k := key(7)
	m := store.Meta{Method: http.MethodGet, URL: "http://o/x", Size: 3, Vary: []string{"accept"}}
	m.Status = 404
	m.Header = http.Header{"X-A": {"1", "2"}}
	m.ResponseTime = time.Date(2026, 9, 19, 12, 0, 0, 123, time.UTC)
	b, err := encodeMeta(k, m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMeta(b, k.String())
	if err != nil {
		t.Fatal(err)
	}
	if !metaEqual(m, got) {
		t.Fatalf("round trip:\n%+v\n%+v", m, got)
	}
	if _, err := decodeMeta(b, key(8).String()); err == nil {
		t.Fatal("meta accepted under another key")
	}
	if _, err := decodeMeta(bytes.Repeat([]byte("x"), maxMetaBytes+1), k.String()); err == nil {
		t.Fatal("over-cap meta accepted")
	}
}

// keyFromHex parses a 64-hex-char stem; the fuzz base need not be one.
func keyFromHex(s string) (k [32]byte, ok bool) {
	if len(s) != 64 || !isHex(s) {
		return k, false
	}
	for i := range 32 {
		k[i] = unhex(s[2*i])<<4 | unhex(s[2*i+1])
	}
	return k, true
}

func unhex(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

// metaEqual compares two metas the way the store cares: times by instant,
// nil and empty collections alike.
func metaEqual(a, b store.Meta) bool {
	norm := func(m store.Meta) store.Meta {
		m.RequestTime, m.ResponseTime, m.LastModified = m.RequestTime.UTC(), m.ResponseTime.UTC(), m.LastModified.UTC()
		if len(m.Header) == 0 {
			m.Header = nil
		}
		if len(m.Vary) == 0 {
			m.Vary = nil
		}
		if len(m.Tags) == 0 {
			m.Tags = nil
		}
		return m
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
