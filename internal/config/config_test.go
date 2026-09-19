package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setBase gives Load the one required key plus a clean slate for the keys a case
// may override; t.Setenv makes every test serial, so no t.Parallel here.
func setBase(t *testing.T, env map[string]string) {
	t.Helper()
	t.Setenv("EDGECACHE_ORIGIN", "http://127.0.0.1:8082")
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setBase(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Addr", cfg.Addr, DefaultAddr},
		{"AdminAddr", cfg.AdminAddr, DefaultAdminAddr},
		{"Origin", cfg.Origin.String(), "http://127.0.0.1:8082"},
		{"OriginHost", cfg.OriginHost, ""},
		{"OriginHeaderTimeout", cfg.OriginHeaderTimeout, 10 * time.Second},
		{"OriginTimeout", cfg.OriginTimeout, 60 * time.Second},
		{"OriginMaxConns", cfg.OriginMaxConns, 128},
		{"MemBytes", cfg.MemBytes, 512 * MiB},
		{"MemShards", cfg.MemShards, 16},
		{"DiskDir", cfg.DiskDir, ""},
		{"DiskBytes", cfg.DiskBytes, 5 * GiB},
		{"MaxObjectBytes", cfg.MaxObjectBytes, 64 * MiB},
		{"MemMaxObjectBytes", cfg.MemMaxObjectBytes, 1 * MiB},
		{"MaxVariants", cfg.MaxVariants, 8},
		{"HeuristicFraction", cfg.HeuristicFraction, 0.1},
		{"HeuristicMax", cfg.HeuristicMax, 24 * time.Hour},
		{"DefaultSWR", cfg.DefaultSWR, time.Duration(0)},
		{"DefaultSIE", cfg.DefaultSIE, time.Duration(0)},
		{"CacheAuthorization", cfg.CacheAuthorization, false},
		{"OrphanFetch", cfg.OrphanFetch, true},
		{"OrphanMaxBytes", cfg.OrphanMaxBytes, 8 * MiB},
		{"PurgeToken", cfg.PurgeToken, ""},
		{"PurgeVerbose", cfg.PurgeVerbose, false},
		{"TagHeaders", strings.Join(cfg.TagHeaders, ","), "Cache-Tag,Surrogate-Key"},
		{"JanitorInterval", cfg.JanitorInterval, 30 * time.Second},
		{"CollapseLeadTimeout", cfg.CollapseLeadTimeout, 60 * time.Second},
		{"RequestTimeout", cfg.RequestTimeout, 60 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 15 * time.Second},
		{"LogFormat", cfg.LogFormat, "json"},
		{"OTELEnabled", cfg.OTELEnabled, false},
		{"OTELEndpoint", cfg.OTELEndpoint, "http://localhost:4317"},
		{"ServiceName", cfg.ServiceName, "edgecache"},
		{"DiskEnabled", cfg.DiskEnabled(), false},
		{"PurgeEnabled", cfg.PurgeEnabled(), false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadTable(t *testing.T) {
	tmp := t.TempDir()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string // substrings that must all appear; empty means success
		check   func(t *testing.T, c Config)
	}{
		{
			name: "sizes durations and lists parse",
			env: map[string]string{
				"EDGECACHE_MEM_BYTES":            "64MiB",
				"EDGECACHE_MEM_MAX_OBJECT_BYTES": "2M",
				"EDGECACHE_MAX_OBJECT_BYTES":     "1048576",
				"EDGECACHE_ORIGIN_TIMEOUT":       "90",
				"EDGECACHE_DEFAULT_SWR":          "5s",
				"EDGECACHE_TAG_HEADERS":          " cache-tag , x-tags ",
				"EDGECACHE_MEM_SHARDS":           "4",
				"EDGECACHE_HEURISTIC_FRACTION":   "0.25",
				"EDGECACHE_ORIGIN_HOST":          "origin.internal:8080",
				"EDGECACHE_PURGE_TOKEN":          "s3cret",
				"EDGECACHE_PURGE_VERBOSE":        "true",
			},
			check: func(t *testing.T, c Config) {
				t.Helper()
				if c.MemBytes != 64*MiB || c.MemMaxObjectBytes != 2*MiB || c.MaxObjectBytes != MiB {
					t.Errorf("bytes = %d/%d/%d", c.MemBytes, c.MemMaxObjectBytes, c.MaxObjectBytes)
				}
				if c.OriginTimeout != 90*time.Second || c.DefaultSWR != 5*time.Second {
					t.Errorf("durations = %s/%s", c.OriginTimeout, c.DefaultSWR)
				}
				if got := strings.Join(c.TagHeaders, ","); got != "Cache-Tag,X-Tags" {
					t.Errorf("TagHeaders = %q", got)
				}
				if c.MemShards != 4 || c.HeuristicFraction != 0.25 || c.OriginHost != "origin.internal:8080" {
					t.Errorf("shards/fraction/host = %d/%g/%q", c.MemShards, c.HeuristicFraction, c.OriginHost)
				}
				if !c.PurgeEnabled() || !c.PurgeVerbose {
					t.Errorf("purge = %v/%v", c.PurgeEnabled(), c.PurgeVerbose)
				}
			},
		},
		{
			name: "disk dir is created and probed",
			env:  map[string]string{"EDGECACHE_DISK_DIR": filepath.Join(tmp, "cache", "nested"), "EDGECACHE_DISK_BYTES": "1GiB"},
			check: func(t *testing.T, c Config) {
				t.Helper()
				if !c.DiskEnabled() || c.DiskBytes != GiB {
					t.Errorf("disk = %v/%d", c.DiskEnabled(), c.DiskBytes)
				}
			},
		},
		{
			name: "non-loopback admin with a token is fine",
			env:  map[string]string{"EDGECACHE_ADMIN_ADDR": "0.0.0.0:9101", "EDGECACHE_PURGE_TOKEN": "t"},
			check: func(t *testing.T, c Config) {
				t.Helper()
				if c.AdminAddr != "0.0.0.0:9101" {
					t.Errorf("AdminAddr = %q", c.AdminAddr)
				}
			},
		},
		{name: "origin missing", env: map[string]string{"EDGECACHE_ORIGIN": ""}, wantErr: []string{"EDGECACHE_ORIGIN is required"}},
		{name: "origin bad scheme", env: map[string]string{"EDGECACHE_ORIGIN": "ftp://host"}, wantErr: []string{"scheme must be http or https"}},
		{name: "origin no host", env: map[string]string{"EDGECACHE_ORIGIN": "http://"}, wantErr: []string{"host is required"}},
		{name: "origin with query", env: map[string]string{"EDGECACHE_ORIGIN": "http://h:1/?x=1"}, wantErr: []string{"query and fragment"}},
		{name: "origin unparsable", env: map[string]string{"EDGECACHE_ORIGIN": "http://[::1"}, wantErr: []string{"EDGECACHE_ORIGIN"}},
		{name: "bad addr", env: map[string]string{"EDGECACHE_ADDR": "8081"}, wantErr: []string{"EDGECACHE_ADDR"}},
		{name: "bad admin addr", env: map[string]string{"EDGECACHE_ADMIN_ADDR": "nope"}, wantErr: []string{"EDGECACHE_ADMIN_ADDR"}},
		{name: "same addrs", env: map[string]string{"EDGECACHE_ADMIN_ADDR": DefaultAddr}, wantErr: []string{"must differ"}},
		{name: "mem too small", env: map[string]string{"EDGECACHE_MEM_BYTES": "512KiB", "EDGECACHE_MEM_MAX_OBJECT_BYTES": "1KiB"}, wantErr: []string{"EDGECACHE_MEM_BYTES must be >= 1MiB"}},
		{name: "object cap too small", env: map[string]string{"EDGECACHE_MAX_OBJECT_BYTES": "1000"}, wantErr: []string{"EDGECACHE_MAX_OBJECT_BYTES must be >= 1MiB"}},
		{name: "disk too small", env: map[string]string{"EDGECACHE_DISK_DIR": tmp, "EDGECACHE_DISK_BYTES": "1"}, wantErr: []string{"EDGECACHE_DISK_BYTES must be >= 1MiB"}},
		{name: "disk size ignored when disabled", env: map[string]string{"EDGECACHE_DISK_BYTES": "1"}, check: func(t *testing.T, c Config) {
			t.Helper()
			if c.DiskBytes != 1 {
				t.Errorf("DiskBytes = %d", c.DiskBytes)
			}
		}},
		{name: "mem object cap above mem", env: map[string]string{"EDGECACHE_MEM_BYTES": "1MiB", "EDGECACHE_MEM_MAX_OBJECT_BYTES": "2MiB"}, wantErr: []string{"must be <= EDGECACHE_MEM_BYTES"}},
		{name: "mem object cap zero", env: map[string]string{"EDGECACHE_MEM_MAX_OBJECT_BYTES": "0"}, wantErr: []string{"EDGECACHE_MEM_MAX_OBJECT_BYTES must be >= 1"}},
		{name: "bad size unit", env: map[string]string{"EDGECACHE_MEM_BYTES": "12parsecs"}, wantErr: []string{"EDGECACHE_MEM_BYTES", "unknown unit"}},
		{name: "shards not power of two", env: map[string]string{"EDGECACHE_MEM_SHARDS": "12"}, wantErr: []string{"power of two"}},
		{name: "shards too many", env: map[string]string{"EDGECACHE_MEM_SHARDS": "512"}, wantErr: []string{"power of two"}},
		{name: "shards zero", env: map[string]string{"EDGECACHE_MEM_SHARDS": "0"}, wantErr: []string{"power of two"}},
		{name: "variants zero", env: map[string]string{"EDGECACHE_MAX_VARIANTS": "0"}, wantErr: []string{"EDGECACHE_MAX_VARIANTS"}},
		{name: "max conns zero", env: map[string]string{"EDGECACHE_ORIGIN_MAX_CONNS": "0"}, wantErr: []string{"EDGECACHE_ORIGIN_MAX_CONNS"}},
		{name: "bad int", env: map[string]string{"EDGECACHE_ORIGIN_MAX_CONNS": "many"}, wantErr: []string{"EDGECACHE_ORIGIN_MAX_CONNS"}},
		{name: "fraction zero", env: map[string]string{"EDGECACHE_HEURISTIC_FRACTION": "0"}, wantErr: []string{"(0,1]"}},
		{name: "fraction above one", env: map[string]string{"EDGECACHE_HEURISTIC_FRACTION": "1.5"}, wantErr: []string{"(0,1]"}},
		{name: "bad float", env: map[string]string{"EDGECACHE_HEURISTIC_FRACTION": "tenth"}, wantErr: []string{"EDGECACHE_HEURISTIC_FRACTION"}},
		{name: "duration zero", env: map[string]string{"EDGECACHE_JANITOR_INTERVAL": "0s"}, wantErr: []string{"EDGECACHE_JANITOR_INTERVAL must be > 0"}},
		{name: "bad duration", env: map[string]string{"EDGECACHE_COLLAPSE_LEAD_TIMEOUT": "forever"}, wantErr: []string{"EDGECACHE_COLLAPSE_LEAD_TIMEOUT"}},
		{name: "negative swr", env: map[string]string{"EDGECACHE_DEFAULT_SWR": "-1s"}, wantErr: []string{"EDGECACHE_DEFAULT_SWR must be >= 0"}},
		{name: "header timeout above total", env: map[string]string{"EDGECACHE_ORIGIN_HEADER_TIMEOUT": "2m"}, wantErr: []string{"EDGECACHE_ORIGIN_HEADER_TIMEOUT must be <="}},
		{name: "orphan max zero while enabled", env: map[string]string{"EDGECACHE_ORPHAN_MAX_BYTES": "0"}, wantErr: []string{"EDGECACHE_ORPHAN_MAX_BYTES"}},
		{name: "orphan max zero while disabled", env: map[string]string{"EDGECACHE_ORPHAN_MAX_BYTES": "0", "EDGECACHE_ORPHAN_FETCH": "false"}, check: func(t *testing.T, c Config) {
			t.Helper()
			if c.OrphanFetch {
				t.Error("OrphanFetch should be false")
			}
		}},
		{name: "bad bool", env: map[string]string{"EDGECACHE_ORPHAN_FETCH": "maybe"}, wantErr: []string{"EDGECACHE_ORPHAN_FETCH"}},
		{name: "empty tag headers", env: map[string]string{"EDGECACHE_TAG_HEADERS": " , "}, wantErr: []string{"at least one header"}},
		{name: "bad tag header", env: map[string]string{"EDGECACHE_TAG_HEADERS": "Cache Tag"}, wantErr: []string{"not a header name"}},
		{name: "non-loopback admin needs token", env: map[string]string{"EDGECACHE_ADMIN_ADDR": ":9101"}, wantErr: []string{"EDGECACHE_PURGE_TOKEN is required"}},
		{name: "bad origin host override", env: map[string]string{"EDGECACHE_ORIGIN_HOST": "a b"}, wantErr: []string{"EDGECACHE_ORIGIN_HOST"}},
		{name: "bad log format", env: map[string]string{"LOG_FORMAT": "yaml"}, wantErr: []string{"LOG_FORMAT"}},
		{name: "bad log level", env: map[string]string{"LOG_LEVEL": "loud"}, wantErr: []string{"LOG_LEVEL"}},
		{
			name: "several errors at once",
			env:  map[string]string{"EDGECACHE_ORIGIN": "", "LOG_FORMAT": "yaml", "EDGECACHE_MEM_SHARDS": "3", "EDGECACHE_MEM_BYTES": "1x"},
			wantErr: []string{
				"EDGECACHE_ORIGIN is required", "LOG_FORMAT", "power of two", "EDGECACHE_MEM_BYTES",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBase(t, tt.env)
			cfg, err := Load()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				tt.check(t, cfg)
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want substrings %q", tt.wantErr)
			}
			for _, sub := range tt.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err = %v, want substring %q", err, sub)
				}
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"1KiB", KiB, false},
		{"2k", 2 * KiB, false},
		{"512MiB", 512 * MiB, false},
		{"512 MB", 512 * MiB, false},
		{"5GiB", 5 * GiB, false},
		{"1T", TiB, false},
		{"64m", 64 * MiB, false},
		{"", 0, true},
		{"MiB", 0, true},
		{"1.5GiB", 0, true},
		{"1PiB", 0, true},
		{"-1", -1, false},
		{"9000000000000000000GiB", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBytes(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBytes(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9101", true},
		{"[::1]:9101", true},
		{"localhost:9101", true},
		{"LOCALHOST:9101", true},
		{"127.5.5.5:1", true},
		{":9101", false},
		{"0.0.0.0:9101", false},
		{"[::]:9101", false},
		{"10.0.0.2:9101", false},
		{"example.com:9101", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := isLoopback(tt.addr); got != tt.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// TestValidateRedactsOriginPassword pins that a userinfo password in
// EDGECACHE_ORIGIN never reaches the joined validation error (which is logged).
func TestValidateRedactsOriginPassword(t *testing.T) {
	setBase(t, map[string]string{"EDGECACHE_ORIGIN": "ftp://user:hunter2@origin.test/base?x=1"})
	_, err := Load()
	if err == nil {
		t.Fatal("expected validation errors for an ftp origin with a query")
	}
	msg := err.Error()
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("error leaks the origin password: %q", msg)
	}
	if !strings.Contains(msg, "user:xxxxx@origin.test") {
		t.Fatalf("error should name the redacted origin: %q", msg)
	}
}
