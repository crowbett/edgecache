// Command slow-origin is the benchmark origin: an HTTP server with configurable
// latency and jitter, body size, Cache-Control mode and keyspace serving
// GET /obj/{n}, plus GET /admin/hits (JSON counters) and DELETE /admin/hits
// (reset). Every option is an environment variable (ORIGIN_ADDR,
// ORIGIN_LATENCY, ORIGIN_JITTER, ORIGIN_BODY_BYTES, ORIGIN_CACHE_CONTROL,
// ORIGIN_KEYSPACE) that the flag of the same name overrides.
//
// ORIGIN_CACHE_CONTROL modes: a literal Cache-Control value such as
// "max-age=60" or "no-store", or one of the named presets "etag" (max-age=1
// with validators, so the proxy revalidates and gets 304s), "vary" (max-age=60
// plus Vary: Accept-Encoding) and "swr" (max-age=1, stale-while-revalidate=30,
// stale-if-error=60). Every mode sends ETag and Last-Modified and answers a
// matching If-None-Match / If-Modified-Since with 304.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// options is the parsed environment; defaults are the ones docs/PLAN.md documents.
type options struct {
	addr         string        // ORIGIN_ADDR
	latency      time.Duration // ORIGIN_LATENCY
	jitter       time.Duration // ORIGIN_JITTER
	bodyBytes    int           // ORIGIN_BODY_BYTES
	cacheControl string        // ORIGIN_CACHE_CONTROL
	keyspace     int           // ORIGIN_KEYSPACE
}

// Config is what Handler needs; a test builds one directly.
type Config struct {
	// Latency is the base delay before every /obj response; Jitter widens it
	// uniformly to [Latency-Jitter, Latency+Jitter] (clamped at 0).
	Latency, Jitter time.Duration
	// BodyBytes is the response body length.
	BodyBytes int
	// CacheControl is a literal Cache-Control value or a preset: etag | vary | swr.
	CacheControl string
	// Keyspace is the number of objects: /obj/0 .. /obj/{Keyspace-1}.
	Keyspace int
	// Start is the Last-Modified of every object; zero means time.Now().
	Start time.Time
}

// Preset names accepted in CacheControl.
const (
	ModeETag = "etag"
	ModeVary = "vary"
	ModeSWR  = "swr"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	opts, err := loadOptions()
	if err != nil {
		return err
	}
	flag.StringVar(&opts.addr, "addr", opts.addr, "listen address (ORIGIN_ADDR)")
	flag.DurationVar(&opts.latency, "latency", opts.latency, "base response latency (ORIGIN_LATENCY)")
	flag.DurationVar(&opts.jitter, "jitter", opts.jitter, "latency jitter, +/- (ORIGIN_JITTER)")
	flag.IntVar(&opts.bodyBytes, "body", opts.bodyBytes, "response body bytes (ORIGIN_BODY_BYTES)")
	flag.StringVar(&opts.cacheControl, "cc", opts.cacheControl, "Cache-Control value or preset etag|vary|swr (ORIGIN_CACHE_CONTROL)")
	flag.IntVar(&opts.keyspace, "keyspace", opts.keyspace, "number of objects under /obj/ (ORIGIN_KEYSPACE)")
	flag.Parse()
	if err := opts.validate(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &http.Server{
		Addr: opts.addr,
		Handler: Handler(Config{
			Latency:      opts.latency,
			Jitter:       opts.jitter,
			BodyBytes:    opts.bodyBytes,
			CacheControl: opts.cacheControl,
			Keyspace:     opts.keyspace,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("slow-origin listening", slog.String("addr", opts.addr), slog.Duration("latency", opts.latency),
			slog.Duration("jitter", opts.jitter), slog.Int("body_bytes", opts.bodyBytes),
			slog.String("cache_control", opts.cacheControl), slog.Int("keyspace", opts.keyspace))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// server is the handler state: counters and the precomputed body filler.
type server struct {
	cfg      Config
	cc       string
	vary     bool
	filler   []byte
	start    time.Time
	lastMod  string
	total    atomic.Int64
	notMod   atomic.Int64
	perKeyMu sync.Mutex
	perKey   map[int]int64
}

// Handler builds the origin handler for cfg so a benchmark or an in-process
// test can mount it without the process wrapper.
func Handler(cfg Config) http.Handler {
	if cfg.Keyspace < 1 {
		cfg.Keyspace = 1
	}
	if cfg.BodyBytes < 0 {
		cfg.BodyBytes = 0
	}
	if cfg.Start.IsZero() {
		cfg.Start = time.Now()
	}
	s := &server{
		cfg:    cfg,
		filler: make([]byte, cfg.BodyBytes),
		start:  cfg.Start.UTC().Truncate(time.Second),
		perKey: make(map[int]int64),
	}
	s.lastMod = s.start.Format(http.TimeFormat)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789\n"
	for i := range s.filler {
		s.filler[i] = alphabet[i%len(alphabet)]
	}
	switch cfg.CacheControl {
	case ModeETag:
		s.cc = "max-age=1"
	case ModeVary:
		s.cc, s.vary = "max-age=60", true
	case ModeSWR:
		s.cc = "max-age=1, stale-while-revalidate=30, stale-if-error=60"
	default:
		s.cc = cfg.CacheControl
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /obj/{n}", s.obj)
	mux.HandleFunc("/obj/{n}", s.mutate)
	mux.HandleFunc("GET /admin/hits", s.hits)
	mux.HandleFunc("DELETE /admin/hits", s.reset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// obj serves one object after the configured delay.
func (s *server) obj(w http.ResponseWriter, r *http.Request) {
	n, ok := s.key(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.sleep(r.Context()) {
		return
	}
	s.total.Add(1)
	s.perKeyMu.Lock()
	s.perKey[n]++
	s.perKeyMu.Unlock()

	etag := `"` + strconv.Itoa(n) + "-" + strconv.Itoa(s.cfg.BodyBytes) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Last-Modified", s.lastMod)
	if s.cc != "" {
		h.Set("Cache-Control", s.cc)
	}
	if s.vary {
		h.Set("Vary", "Accept-Encoding")
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Origin-Key", strconv.Itoa(n))

	if s.notModified(r, etag) {
		s.notMod.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	head := "key=" + strconv.Itoa(n) + "\n"
	body := s.filler
	if len(head) >= len(body) {
		head = head[:len(body)]
		body = nil
	} else {
		body = body[len(head):]
	}
	h.Set("Content-Length", strconv.Itoa(len(head)+len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(head))
	_, _ = w.Write(body)
}

// mutate answers unsafe methods on an object with 204 so the proxy's
// invalidation path can be exercised end to end.
func (s *server) mutate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.key(r); !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// key parses /obj/{n} against the keyspace.
func (s *server) key(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= s.cfg.Keyspace {
		return 0, false
	}
	return n, true
}

// sleep waits latency +/- jitter or until ctx ends; it reports whether the
// request is still live.
func (s *server) sleep(ctx context.Context) bool {
	d := s.cfg.Latency
	if s.cfg.Jitter > 0 {
		d += time.Duration(rand.Int64N(int64(2*s.cfg.Jitter))) - s.cfg.Jitter
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// notModified answers the request's validators against the object's.
func (s *server) notModified(r *http.Request, etag string) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, tag := range splitCSV(inm) {
			if tag == "*" || trimWeak(tag) == trimWeak(etag) {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !s.start.After(t) {
			return true
		}
	}
	return false
}

// hitsSnapshot is the /admin/hits body.
type hitsSnapshot struct {
	Total       int64            `json:"total"`
	NotModified int64            `json:"not_modified"`
	Keys        map[string]int64 `json:"keys"`
}

// hits reports the request counters as JSON.
func (s *server) hits(w http.ResponseWriter, _ *http.Request) {
	snap := hitsSnapshot{Total: s.total.Load(), NotModified: s.notMod.Load(), Keys: map[string]int64{}}
	s.perKeyMu.Lock()
	for k, v := range s.perKey {
		snap.Keys[strconv.Itoa(k)] = v
	}
	s.perKeyMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snap)
}

// reset zeroes the counters (between benchmark rows).
func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.total.Store(0)
	s.notMod.Store(0)
	s.perKeyMu.Lock()
	clear(s.perKey)
	s.perKeyMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func splitCSV(v string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			if t := trimSpace(v[start:i]); t != "" {
				out = append(out, t)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func trimWeak(tag string) string {
	if len(tag) > 2 && tag[0] == 'W' && tag[1] == '/' {
		return tag[2:]
	}
	return tag
}

// loadOptions reads the ORIGIN_* environment, joining every parse error.
func loadOptions() (options, error) {
	o := options{
		addr:         getenv("ORIGIN_ADDR", ":8082"),
		latency:      50 * time.Millisecond,
		jitter:       10 * time.Millisecond,
		bodyBytes:    16384,
		cacheControl: getenv("ORIGIN_CACHE_CONTROL", "max-age=60"),
		keyspace:     1000,
	}
	var errs []error
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"ORIGIN_LATENCY", &o.latency}, {"ORIGIN_JITTER", &o.jitter}} {
		if v := os.Getenv(d.key); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d.key, err))
				continue
			}
			*d.dst = parsed
		}
	}
	for _, n := range []struct {
		key string
		dst *int
	}{{"ORIGIN_BODY_BYTES", &o.bodyBytes}, {"ORIGIN_KEYSPACE", &o.keyspace}} {
		if v := os.Getenv(n.key); v != "" {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n.key, err))
				continue
			}
			*n.dst = parsed
		}
	}
	if err := errors.Join(errs...); err != nil {
		return options{}, err
	}
	return o, o.validate()
}

// validate checks the ranges after env and flags are merged.
func (o options) validate() error {
	var errs []error
	if o.keyspace < 1 {
		errs = append(errs, errors.New("ORIGIN_KEYSPACE must be >= 1"))
	}
	if o.bodyBytes < 0 {
		errs = append(errs, errors.New("ORIGIN_BODY_BYTES must be >= 0"))
	}
	if o.latency < 0 || o.jitter < 0 {
		errs = append(errs, errors.New("ORIGIN_LATENCY and ORIGIN_JITTER must be >= 0"))
	}
	return errors.Join(errs...)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
