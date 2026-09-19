package config

import (
	"strings"
	"testing"
)

// FuzzParseBytes: the size parser never panics, never overflows (an accepted
// value divides back to its integer part) and a value it prints round trips.
func FuzzParseBytes(f *testing.F) {
	for _, s := range []string{
		"", "0", "1", "512MiB", "5GiB", "64MiB", "1k", "1K", "1kb", "1KB", "1 GiB", "1TiB", "1B", "b", "-1",
		"-5MiB", "9223372036854775807", "9223372036854775808", "8EiB", "1.5GiB", "0x10", " 12 ", "12 mib ", "1MiBx",
		"8796093022208MiB", "4194304TiB", "4194305TiB",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseBytes(s)
		if err != nil {
			if n != 0 {
				t.Fatalf("value %d returned alongside error %v", n, err)
			}
			return
		}
		trimmed := strings.TrimSpace(s)
		i := len(trimmed)
		for i > 0 && (trimmed[i-1] < '0' || trimmed[i-1] > '9') {
			i--
		}
		num, suffix := trimmed[:i], strings.ToLower(strings.TrimSpace(trimmed[i:]))
		mult := map[string]int64{
			"": 1, "b": 1, "k": KiB, "kib": KiB, "kb": KiB, "m": MiB, "mib": MiB, "mb": MiB,
			"g": GiB, "gib": GiB, "gb": GiB, "t": TiB, "tib": TiB, "tb": TiB,
		}[suffix]
		if mult == 0 {
			t.Fatalf("accepted unknown unit %q", suffix)
		}
		if n%mult != 0 {
			t.Fatalf("%q -> %d is not a multiple of %d", s, n, mult)
		}
		if n > 0 && (n/mult > 1<<62/mult || n < 0) {
			t.Fatalf("%q -> %d overflowed", s, n)
		}
		if again, err := ParseBytes(num + suffix); err != nil || again != n {
			t.Fatalf("re-parse of %q%q = %d, %v; want %d", num, suffix, again, err, n)
		}
	})
}
