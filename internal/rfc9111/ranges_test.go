package rfc9111

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseRange(t *testing.T) {
	t.Parallel()
	br := func(start, length int64) ByteRange { return ByteRange{Start: start, Length: length} }
	tests := []struct {
		name    string
		in      string
		size    int64
		want    []ByteRange
		wantErr error
	}{
		{"first bytes", "bytes=0-3", 10, []ByteRange{br(0, 4)}, nil},
		{"open ended", "bytes=4-", 10, []ByteRange{br(4, 6)}, nil},
		{"suffix", "bytes=-3", 10, []ByteRange{br(7, 3)}, nil},
		{"suffix longer than body selects all", "bytes=-50", 10, []ByteRange{br(0, 10)}, nil},
		{"last-pos clamped", "bytes=8-100", 10, []ByteRange{br(8, 2)}, nil},
		{"single last byte", "bytes=9-9", 10, []ByteRange{br(9, 1)}, nil},
		{"unit case-insensitive", "Bytes=0-0", 10, []ByteRange{br(0, 1)}, nil},
		{"multi with OWS and empty elements", "bytes= 0-1 , ,\t5-6,", 10, []ByteRange{br(0, 2), br(5, 2)}, nil},
		{"request order kept", "bytes=5-6,0-1", 10, []ByteRange{br(5, 2), br(0, 2)}, nil},
		{"unsatisfiable spec dropped", "bytes=0-1,20-30", 10, []ByteRange{br(0, 2)}, nil},
		{"huge last-pos clamps", "bytes=2-99999999999999999999999", 10, []ByteRange{br(2, 8)}, nil},
		{"huge suffix selects all", "bytes=-99999999999999999999999", 10, []ByteRange{br(0, 10)}, nil},
		{"exactly the whole body twice is too much", "bytes=0-9,0-0", 10, nil, ErrRangeIgnored},
		{"overlap summing past size ignored", "bytes=0-6,3-9", 10, nil, ErrRangeIgnored},
		{"first-pos at size", "bytes=10-", 10, nil, ErrRangeUnsatisfiable},
		{"huge first-pos", "bytes=99999999999999999999999-", 10, nil, ErrRangeUnsatisfiable},
		{"zero suffix", "bytes=-0", 10, nil, ErrRangeUnsatisfiable},
		{"empty representation", "bytes=0-0", 0, nil, ErrRangeUnsatisfiable},
		{"empty representation suffix", "bytes=-5", 0, nil, ErrRangeUnsatisfiable},
		{"unknown unit", "items=0-1", 10, nil, ErrRangeIgnored},
		{"no equals", "bytes 0-1", 10, nil, ErrRangeIgnored},
		{"empty set", "bytes=", 10, nil, ErrRangeIgnored},
		{"only commas", "bytes=,,", 10, nil, ErrRangeIgnored},
		{"no dash", "bytes=5", 10, nil, ErrRangeIgnored},
		{"last before first", "bytes=5-4", 10, nil, ErrRangeIgnored},
		{"negative", "bytes=--1", 10, nil, ErrRangeIgnored},
		{"sign", "bytes=+1-2", 10, nil, ErrRangeIgnored},
		{"inner space", "bytes=1 -2", 10, nil, ErrRangeIgnored},
		{"bare dash", "bytes=-", 10, nil, ErrRangeIgnored},
		{"one bad spec poisons the set", "bytes=0-1,x-2", 10, nil, ErrRangeIgnored},
		{"empty header", "", 10, nil, ErrRangeIgnored},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRange(tc.in, tc.size)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("ParseRange(%q, %d) err = %v, want %v", tc.in, tc.size, err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseRange(%q, %d) = %v, want %v", tc.in, tc.size, got, tc.want)
			}
		})
	}
}

func TestParseRangeTooMany(t *testing.T) {
	t.Parallel()
	s := "bytes=" + strings.Repeat("0-0,", MaxRanges) + "0-0"
	if _, err := ParseRange(s, 1<<20); !errors.Is(err, ErrRangeIgnored) {
		t.Errorf("%d ranges: err = %v, want ErrRangeIgnored", MaxRanges+1, err)
	}
}

func TestContentRange(t *testing.T) {
	t.Parallel()
	if got := (ByteRange{Start: 2, Length: 3}).ContentRange(10); got != "bytes 2-4/10" {
		t.Errorf("ContentRange = %q", got)
	}
	if got := UnsatisfiedRange(10); got != "bytes */10" {
		t.Errorf("UnsatisfiedRange = %q", got)
	}
}

func TestIfRange(t *testing.T) {
	t.Parallel()
	const lm = "Sat, 19 Sep 2026 12:00:00 GMT"
	tests := []struct {
		name               string
		ifRange, etag, lmv string
		want               bool
	}{
		{"absent", "", `"a"`, lm, true},
		{"strong etag match", `"a"`, `"a"`, "", true},
		{"strong etag mismatch", `"b"`, `"a"`, "", false},
		{"weak request tag never matches", `W/"a"`, `W/"a"`, "", false},
		{"weak stored tag never matches", `"a"`, `W/"a"`, "", false},
		{"etag with no stored etag", `"a"`, "", lm, false},
		{"date exact match", lm, `"a"`, lm, true},
		{"date other format same instant", "Saturday, 19-Sep-26 12:00:00 GMT", "", lm, true},
		{"date mismatch", "Sat, 19 Sep 2026 12:00:01 GMT", "", lm, false},
		{"date with no stored Last-Modified", lm, `"a"`, "", false},
		{"garbage", "yesterday", `"a"`, lm, false},
		{"unterminated tag", `"a`, `"a`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IfRange(tc.ifRange, tc.etag, tc.lmv); got != tc.want {
				t.Errorf("IfRange(%q, %q, %q) = %v, want %v", tc.ifRange, tc.etag, tc.lmv, got, tc.want)
			}
		})
	}
}

// FuzzParseRange asserts ParseRange never panics and that every accepted
// result is internally consistent: at least one range, each non-empty and
// inside the representation, never more bytes in total than the body, at most
// MaxRanges, and parsing is deterministic.
func FuzzParseRange(f *testing.F) {
	for _, s := range []string{
		"bytes=0-3", "bytes=-3", "bytes=4-", "bytes=0-1,5-6", "bytes=0-0,-1", "Bytes = 1-2", "bytes=,,,",
		"bytes=5-4", "bytes=99999999999999999999-", "bytes=-99999999999999999999", "items=0-1", "bytes",
		"bytes=0-9,0-0", "bytes= 0 - 1", "bytes=-0", "bytes=1-1,2-2,3-3,4-4",
	} {
		f.Add(s, int64(10))
		f.Add(s, int64(0))
		f.Add(s, int64(1<<40))
	}
	f.Fuzz(func(t *testing.T, s string, size int64) {
		got, err := ParseRange(s, size)
		again, err2 := ParseRange(s, size)
		if (err == nil) != (err2 == nil) || (err != nil && !errors.Is(err2, err)) {
			t.Fatalf("non-deterministic error: %v then %v", err, err2)
		}
		if !reflect.DeepEqual(got, again) {
			t.Fatalf("non-deterministic result: %v then %v", got, again)
		}
		if err != nil {
			if !errors.Is(err, ErrRangeIgnored) && !errors.Is(err, ErrRangeUnsatisfiable) {
				t.Fatalf("unexpected error %v", err)
			}
			if got != nil {
				t.Fatalf("error %v with ranges %v", err, got)
			}
			return
		}
		if len(got) == 0 || len(got) > MaxRanges {
			t.Fatalf("%d ranges", len(got))
		}
		var total int64
		for _, r := range got {
			if r.Start < 0 || r.Length <= 0 || r.Start >= size || r.Length > size-r.Start {
				t.Fatalf("range %+v outside a %d-byte body", r, size)
			}
			total += r.Length
			if total > size {
				t.Fatalf("ranges total %d > size %d", total, size)
			}
		}
	})
}
