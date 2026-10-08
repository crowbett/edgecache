// Package config loads runtime configuration from environment variables. All
// validation happens here so a bad deploy fails at startup with the full list of
// problems (errors.Join), aigw style. No globals: Config is passed by value.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crowbett/edgecache/internal/trustproxy"
)

// Byte-size units accepted by ParseBytes ("512MiB", "5GiB", "1024", ...).
const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
	TiB int64 = 1 << 40
)

// Defaults that the README documents.
const (
	DefaultAddr                = "127.0.0.1:8081"
	DefaultAdminAddr           = "127.0.0.1:9101"
	DefaultOriginHeaderTimeout = 10 * time.Second
	DefaultOriginTimeout       = 60 * time.Second
	DefaultOriginMaxConns      = 128
	DefaultMemBytes            = 512 * MiB
	DefaultMemShards           = 16
	DefaultDiskBytes           = 5 * GiB
	DefaultMaxObjectBytes      = 64 * MiB
	DefaultMemMaxObjectBytes   = 1 * MiB
	DefaultMaxVariants         = 8
	DefaultHeuristicFraction   = 0.1
	DefaultHeuristicMax        = 24 * time.Hour
	DefaultOrphanMaxBytes      = 8 * MiB
	DefaultTagHeaders          = "Cache-Tag,Surrogate-Key"
	DefaultJanitorInterval     = 30 * time.Second
	DefaultCollapseLeadTimeout = 60 * time.Second
	DefaultCollapseStreamBytes = 8 * MiB
	DefaultRequestTimeout      = 60 * time.Second
	DefaultShutdownTimeout     = 15 * time.Second
)

// minBudget is the floor for every byte budget (memory, disk, object cap).
const minBudget = 1 * MiB

// Config holds every tunable the service reads at startup. Field comments name
// the environment key; the table in docs/PLAN.md is the source of truth.
type Config struct {
	ServiceName     string        // SERVICE_NAME
	Env             string        // APP_ENV: dev | staging | prod
	Addr            string        // EDGECACHE_ADDR: proxy listener
	AdminAddr       string        // EDGECACHE_ADMIN_ADDR: admin + metrics + pprof listener
	LogLevel        slog.Level    // LOG_LEVEL
	LogFormat       string        // LOG_FORMAT: json | text
	RequestTimeout  time.Duration // REQUEST_TIMEOUT: admin mux only (the proxy has no TimeoutHandler)
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT: stage-2 proxy drain budget

	// OTELEnabled turns on OTLP trace + metric export (OTEL_ENABLED=true).
	OTELEnabled bool
	// OTELEndpoint is the OTLP/gRPC collector URL (OTEL_EXPORTER_OTLP_ENDPOINT).
	OTELEndpoint string

	// Origin is the upstream base URL (EDGECACHE_ORIGIN, required, http or https).
	Origin *url.URL
	// OriginHost overrides the Host header sent upstream (EDGECACHE_ORIGIN_HOST); empty keeps Origin's host.
	OriginHost          string
	OriginHeaderTimeout time.Duration // EDGECACHE_ORIGIN_HEADER_TIMEOUT: wait for response headers
	OriginTimeout       time.Duration // EDGECACHE_ORIGIN_TIMEOUT: whole fetch budget
	OriginMaxConns      int           // EDGECACHE_ORIGIN_MAX_CONNS: transport pool per host

	MemBytes  int64  // EDGECACHE_MEM_BYTES: memory tier budget
	MemShards int    // EDGECACHE_MEM_SHARDS: power of two in [1,256]
	DiskDir   string // EDGECACHE_DISK_DIR: empty disables the disk tier
	DiskBytes int64  // EDGECACHE_DISK_BYTES: disk tier budget

	MaxObjectBytes    int64 // EDGECACHE_MAX_OBJECT_BYTES: store admission cap
	MemMaxObjectBytes int64 // EDGECACHE_MEM_MAX_OBJECT_BYTES: memory admission cap (<= MemBytes)
	MaxVariants       int   // EDGECACHE_MAX_VARIANTS: Vary variants per primary key (LRU)

	HeuristicFraction  float64       // EDGECACHE_HEURISTIC_FRACTION: (0,1] of (Date - Last-Modified)
	HeuristicMax       time.Duration // EDGECACHE_HEURISTIC_MAX: heuristic freshness cap
	DefaultSWR         time.Duration // EDGECACHE_DEFAULT_SWR: stale-while-revalidate when the origin sends none
	DefaultSIE         time.Duration // EDGECACHE_DEFAULT_SIE: stale-if-error when the origin sends none
	CacheAuthorization bool          // EDGECACHE_CACHE_AUTHORIZATION: override RFC 9111 §3.5

	OrphanFetch    bool  // EDGECACHE_ORPHAN_FETCH: finish a fetch after the client disconnects
	OrphanMaxBytes int64 // EDGECACHE_ORPHAN_MAX_BYTES: how far to keep filling the store after that

	PurgeToken   string   // EDGECACHE_PURGE_TOKEN: X-Purge-Token value; empty disables PURGE on the proxy listener
	PurgeVerbose bool     // EDGECACHE_PURGE_VERBOSE: list purged keys in the response
	TagHeaders   []string // EDGECACHE_TAG_HEADERS: response headers read as surrogate keys (canonical form)

	JanitorInterval     time.Duration // EDGECACHE_JANITOR_INTERVAL: tombstone sweep + gauge refresh tick
	CollapseLeadTimeout time.Duration // EDGECACHE_COLLAPSE_LEAD_TIMEOUT: collapse leader budget
	// CollapseStreamBytes is how much of a collapsed miss is retained so waiters
	// can serve it while the leader downloads (EDGECACHE_COLLAPSE_STREAM_BYTES);
	// 0 makes every waiter wait for the commit.
	CollapseStreamBytes int64

	// TrustedProxies are the peers whose X-Forwarded-Proto/-For/-Host and
	// Forwarded headers are honoured (EDGECACHE_TRUSTED_PROXIES, comma-separated
	// CIDRs or IPs); empty trusts nobody.
	TrustedProxies []netip.Prefix
	// RangeFill starts a background full GET when a Range request misses, so the
	// next range is cut from a stored 200 (EDGECACHE_RANGE_FILL, default true).
	RangeFill bool
}

// Load reads configuration from the environment, applying the documented
// defaults, and returns every parse and validation problem joined into one error.
func Load() (Config, error) {
	cfg := Config{
		ServiceName:         getenv("SERVICE_NAME", "edgecache"),
		Env:                 getenv("APP_ENV", "dev"),
		Addr:                getenv("EDGECACHE_ADDR", DefaultAddr),
		AdminAddr:           getenv("EDGECACHE_ADMIN_ADDR", DefaultAdminAddr),
		LogFormat:           strings.ToLower(getenv("LOG_FORMAT", "json")),
		RequestTimeout:      DefaultRequestTimeout,
		ShutdownTimeout:     DefaultShutdownTimeout,
		OTELEndpoint:        getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		OriginHost:          os.Getenv("EDGECACHE_ORIGIN_HOST"),
		OriginHeaderTimeout: DefaultOriginHeaderTimeout,
		OriginTimeout:       DefaultOriginTimeout,
		OriginMaxConns:      DefaultOriginMaxConns,
		MemBytes:            DefaultMemBytes,
		MemShards:           DefaultMemShards,
		DiskDir:             os.Getenv("EDGECACHE_DISK_DIR"),
		DiskBytes:           DefaultDiskBytes,
		MaxObjectBytes:      DefaultMaxObjectBytes,
		MemMaxObjectBytes:   DefaultMemMaxObjectBytes,
		MaxVariants:         DefaultMaxVariants,
		HeuristicFraction:   DefaultHeuristicFraction,
		HeuristicMax:        DefaultHeuristicMax,
		OrphanFetch:         true,
		OrphanMaxBytes:      DefaultOrphanMaxBytes,
		RangeFill:           true,
		PurgeToken:          os.Getenv("EDGECACHE_PURGE_TOKEN"),
		TagHeaders:          headerList(getenv("EDGECACHE_TAG_HEADERS", DefaultTagHeaders)),
		JanitorInterval:     DefaultJanitorInterval,
		CollapseLeadTimeout: DefaultCollapseLeadTimeout,
		CollapseStreamBytes: DefaultCollapseStreamBytes,
	}

	var errs []error
	if err := cfg.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"REQUEST_TIMEOUT", &cfg.RequestTimeout},
		{"SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"EDGECACHE_ORIGIN_HEADER_TIMEOUT", &cfg.OriginHeaderTimeout},
		{"EDGECACHE_ORIGIN_TIMEOUT", &cfg.OriginTimeout},
		{"EDGECACHE_HEURISTIC_MAX", &cfg.HeuristicMax},
		{"EDGECACHE_DEFAULT_SWR", &cfg.DefaultSWR},
		{"EDGECACHE_DEFAULT_SIE", &cfg.DefaultSIE},
		{"EDGECACHE_JANITOR_INTERVAL", &cfg.JanitorInterval},
		{"EDGECACHE_COLLAPSE_LEAD_TIMEOUT", &cfg.CollapseLeadTimeout},
	} {
		var err error
		if *d.dst, err = getDuration(d.key, *d.dst); err != nil {
			errs = append(errs, err)
		}
	}
	for _, b := range []struct {
		key string
		dst *int64
	}{
		{"EDGECACHE_MEM_BYTES", &cfg.MemBytes},
		{"EDGECACHE_DISK_BYTES", &cfg.DiskBytes},
		{"EDGECACHE_MAX_OBJECT_BYTES", &cfg.MaxObjectBytes},
		{"EDGECACHE_MEM_MAX_OBJECT_BYTES", &cfg.MemMaxObjectBytes},
		{"EDGECACHE_ORPHAN_MAX_BYTES", &cfg.OrphanMaxBytes},
		{"EDGECACHE_COLLAPSE_STREAM_BYTES", &cfg.CollapseStreamBytes},
	} {
		var err error
		if *b.dst, err = getBytes(b.key, *b.dst); err != nil {
			errs = append(errs, err)
		}
	}
	for _, n := range []struct {
		key string
		dst *int
	}{
		{"EDGECACHE_ORIGIN_MAX_CONNS", &cfg.OriginMaxConns},
		{"EDGECACHE_MEM_SHARDS", &cfg.MemShards},
		{"EDGECACHE_MAX_VARIANTS", &cfg.MaxVariants},
	} {
		var err error
		if *n.dst, err = getInt(n.key, *n.dst); err != nil {
			errs = append(errs, err)
		}
	}
	for _, b := range []struct {
		key string
		dst *bool
	}{
		{"OTEL_ENABLED", &cfg.OTELEnabled},
		{"EDGECACHE_CACHE_AUTHORIZATION", &cfg.CacheAuthorization},
		{"EDGECACHE_ORPHAN_FETCH", &cfg.OrphanFetch},
		{"EDGECACHE_PURGE_VERBOSE", &cfg.PurgeVerbose},
		{"EDGECACHE_RANGE_FILL", &cfg.RangeFill},
	} {
		var err error
		if *b.dst, err = getBool(b.key, *b.dst); err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	if cfg.TrustedProxies, err = trustproxy.Parse(os.Getenv("EDGECACHE_TRUSTED_PROXIES")); err != nil {
		errs = append(errs, fmt.Errorf("EDGECACHE_TRUSTED_PROXIES: %w", err))
	}
	if cfg.HeuristicFraction, err = getFloat("EDGECACHE_HEURISTIC_FRACTION", cfg.HeuristicFraction); err != nil {
		errs = append(errs, err)
	}

	switch raw := os.Getenv("EDGECACHE_ORIGIN"); raw {
	case "":
		errs = append(errs, errors.New("EDGECACHE_ORIGIN is required (upstream base URL)"))
	default:
		u, perr := url.Parse(raw)
		if perr != nil {
			errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN %q: %w", raw, perr))
		} else {
			cfg.Origin = u
		}
	}

	if verr := cfg.Validate(); verr != nil {
		errs = append(errs, verr)
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports every problem at once. It creates DiskDir when the disk tier
// is enabled and probes it with a temp file, so it is the one place with I/O.
func (c Config) Validate() error {
	var errs []error
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text, got %q", c.LogFormat))
	}
	for _, a := range []struct{ key, v string }{
		{"EDGECACHE_ADDR", c.Addr},
		{"EDGECACHE_ADMIN_ADDR", c.AdminAddr},
	} {
		if _, _, err := net.SplitHostPort(a.v); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", a.key, a.v, err))
		}
	}
	if c.Addr != "" && c.Addr == c.AdminAddr {
		errs = append(errs, fmt.Errorf("EDGECACHE_ADDR and EDGECACHE_ADMIN_ADDR must differ, both %q", c.Addr))
	}
	if c.Origin != nil {
		switch {
		case c.Origin.Scheme != "http" && c.Origin.Scheme != "https":
			errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN %q: scheme must be http or https", c.Origin.Redacted()))
		case c.Origin.Host == "":
			errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN %q: host is required", c.Origin.Redacted()))
		}
		if c.Origin.RawQuery != "" || c.Origin.Fragment != "" {
			errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN %q: query and fragment are not allowed", c.Origin.Redacted()))
		}
	}
	if c.OriginHost != "" && strings.ContainsAny(c.OriginHost, " \t/") {
		errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN_HOST %q: must be host[:port]", c.OriginHost))
	}

	for _, d := range []struct {
		key string
		v   time.Duration
	}{
		{"REQUEST_TIMEOUT", c.RequestTimeout},
		{"SHUTDOWN_TIMEOUT", c.ShutdownTimeout},
		{"EDGECACHE_ORIGIN_HEADER_TIMEOUT", c.OriginHeaderTimeout},
		{"EDGECACHE_ORIGIN_TIMEOUT", c.OriginTimeout},
		{"EDGECACHE_HEURISTIC_MAX", c.HeuristicMax},
		{"EDGECACHE_JANITOR_INTERVAL", c.JanitorInterval},
		{"EDGECACHE_COLLAPSE_LEAD_TIMEOUT", c.CollapseLeadTimeout},
	} {
		if d.v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be > 0, got %s", d.key, d.v))
		}
	}
	for _, d := range []struct {
		key string
		v   time.Duration
	}{
		{"EDGECACHE_DEFAULT_SWR", c.DefaultSWR},
		{"EDGECACHE_DEFAULT_SIE", c.DefaultSIE},
	} {
		if d.v < 0 {
			errs = append(errs, fmt.Errorf("%s must be >= 0, got %s", d.key, d.v))
		}
	}
	if c.OriginHeaderTimeout > c.OriginTimeout {
		errs = append(errs, errors.New("EDGECACHE_ORIGIN_HEADER_TIMEOUT must be <= EDGECACHE_ORIGIN_TIMEOUT"))
	}
	if c.OriginMaxConns < 1 {
		errs = append(errs, fmt.Errorf("EDGECACHE_ORIGIN_MAX_CONNS must be >= 1, got %d", c.OriginMaxConns))
	}

	for _, b := range []struct {
		key string
		v   int64
	}{
		{"EDGECACHE_MEM_BYTES", c.MemBytes},
		{"EDGECACHE_MAX_OBJECT_BYTES", c.MaxObjectBytes},
	} {
		if b.v < minBudget {
			errs = append(errs, fmt.Errorf("%s must be >= 1MiB, got %d", b.key, b.v))
		}
	}
	if c.DiskEnabled() && c.DiskBytes < minBudget {
		errs = append(errs, fmt.Errorf("EDGECACHE_DISK_BYTES must be >= 1MiB, got %d", c.DiskBytes))
	}
	if c.MemMaxObjectBytes < 1 {
		errs = append(errs, fmt.Errorf("EDGECACHE_MEM_MAX_OBJECT_BYTES must be >= 1, got %d", c.MemMaxObjectBytes))
	} else if c.MemMaxObjectBytes > c.MemBytes {
		errs = append(errs, errors.New("EDGECACHE_MEM_MAX_OBJECT_BYTES must be <= EDGECACHE_MEM_BYTES"))
	}
	if c.MemShards < 1 || c.MemShards > 256 || c.MemShards&(c.MemShards-1) != 0 {
		errs = append(errs, fmt.Errorf("EDGECACHE_MEM_SHARDS must be a power of two in [1,256], got %d", c.MemShards))
	}
	if c.MaxVariants < 1 {
		errs = append(errs, fmt.Errorf("EDGECACHE_MAX_VARIANTS must be >= 1, got %d", c.MaxVariants))
	}
	if c.HeuristicFraction <= 0 || c.HeuristicFraction > 1 {
		errs = append(errs, fmt.Errorf("EDGECACHE_HEURISTIC_FRACTION must be in (0,1], got %g", c.HeuristicFraction))
	}
	if c.OrphanFetch && c.OrphanMaxBytes < 1 {
		errs = append(errs, fmt.Errorf("EDGECACHE_ORPHAN_MAX_BYTES must be >= 1 when EDGECACHE_ORPHAN_FETCH=true, got %d", c.OrphanMaxBytes))
	}
	if c.OrphanMaxBytes < 0 {
		errs = append(errs, fmt.Errorf("EDGECACHE_ORPHAN_MAX_BYTES must be >= 0, got %d", c.OrphanMaxBytes))
	}
	if c.CollapseStreamBytes < 0 {
		errs = append(errs, fmt.Errorf("EDGECACHE_COLLAPSE_STREAM_BYTES must be >= 0, got %d", c.CollapseStreamBytes))
	}

	if len(c.TagHeaders) == 0 {
		errs = append(errs, errors.New("EDGECACHE_TAG_HEADERS must list at least one header"))
	}
	for _, h := range c.TagHeaders {
		if !validToken(h) {
			errs = append(errs, fmt.Errorf("EDGECACHE_TAG_HEADERS: %q is not a header name", h))
		}
	}

	// The admin listener's POST /admin/purge is unauthenticated by design (loopback
	// only); off loopback it must be behind the same token as PURGE.
	if c.PurgeToken == "" && !isLoopback(c.AdminAddr) {
		errs = append(errs, fmt.Errorf("EDGECACHE_PURGE_TOKEN is required when EDGECACHE_ADMIN_ADDR %q is not loopback", c.AdminAddr))
	}

	if c.DiskEnabled() {
		if err := probeDir(c.DiskDir); err != nil {
			errs = append(errs, fmt.Errorf("EDGECACHE_DISK_DIR %q: %w", c.DiskDir, err))
		}
	}
	return errors.Join(errs...)
}

// IsProd reports whether the service is running in production mode.
func (c Config) IsProd() bool { return c.Env == "prod" }

// DiskEnabled reports whether the disk tier is configured (EDGECACHE_DISK_DIR non-empty).
func (c Config) DiskEnabled() bool { return c.DiskDir != "" }

// PurgeEnabled reports whether PURGE on the proxy listener is allowed
// (EDGECACHE_PURGE_TOKEN non-empty).
func (c Config) PurgeEnabled() bool { return c.PurgeToken != "" }

// ParseBytes parses a byte size: a bare integer, or an integer with a binary
// suffix (K, KiB, M, MiB, G, GiB, T, TiB, case-insensitive, optional B).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	num, suffix := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	var mult int64
	switch suffix {
	case "", "b":
		mult = 1
	case "k", "kib", "kb":
		mult = KiB
	case "m", "mib", "mb":
		mult = MiB
	case "g", "gib", "gb":
		mult = GiB
	case "t", "tib", "tb":
		mult = TiB
	default:
		return 0, fmt.Errorf("size %q: unknown unit %q", s, suffix)
	}
	if n > 0 && n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q: overflows int64", s)
	}
	return n * mult, nil
}

// probeDir creates dir if missing and proves it is writable by creating and
// removing a temp file.
func probeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

// isLoopback reports whether addr binds only a loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validToken accepts an RFC 9110 token (header field name).
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// splitList splits a comma-separated list, trimming and dropping empties.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// headerList is splitList with each name in canonical MIME form (Cache-Tag).
func headerList(s string) []string {
	out := splitList(s)
	for i, h := range out {
		out[i] = textproto.CanonicalMIMEHeaderKey(h)
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func getInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func getBytes(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := ParseBytes(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// getDuration accepts both "30s" and a bare number of seconds.
func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
