package trustproxy

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"empty", "", "", ""},
		{"blanks", " , ,", "", ""},
		{"cidr masked", "10.1.2.3/8", "10.0.0.0/8", ""},
		{"bare v4", "127.0.0.1", "127.0.0.1/32", ""},
		{"bare v6", "::1", "::1/128", ""},
		{"mapped v4 unmapped", "::ffff:192.0.2.1", "192.0.2.1/32", ""},
		{"list", "10.0.0.0/8, 192.168.0.0/16 ,::1", "10.0.0.0/8,192.168.0.0/16,::1/128", ""},
		{"bad cidr", "10.0.0.0/33", "", "10.0.0.0/33"},
		{"bad ip", "localhost", "", "localhost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse(%q) err = %v, want mention of %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if s := New(got).String(); s != tc.want {
				t.Errorf("Parse(%q) = %q, want %q", tc.in, s, tc.want)
			}
		})
	}
}

func TestTrusts(t *testing.T) {
	t.Parallel()
	l := New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")})
	tests := []struct {
		name   string
		list   List
		remote string
		want   bool
	}{
		{"zero list trusts nobody", List{}, "10.0.0.1:1234", false},
		{"inside v4", l, "10.9.8.7:1234", true},
		{"outside v4", l, "192.0.2.1:1234", false},
		{"v6 loopback", l, "[::1]:443", true},
		{"mapped v4", l, "[::ffff:10.0.0.1]:80", true},
		{"bare ip", l, "10.0.0.1", true},
		{"garbage", l, "not-an-address", false},
		{"empty", l, "", false},
		{"hostname", l, "localhost:80", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.list.Trusts(tc.remote); got != tc.want {
				t.Errorf("Trusts(%q) = %v, want %v", tc.remote, got, tc.want)
			}
		})
	}
	if !(List{}).Empty() || l.Empty() {
		t.Error("Empty mismatch")
	}
}

func TestTrustsDoesNotAllocate(t *testing.T) {
	l := New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if n := testing.AllocsPerRun(100, func() { _ = l.Trusts("10.1.2.3:4567") }); n != 0 {
		t.Errorf("Trusts allocates %.0f times", n)
	}
}
