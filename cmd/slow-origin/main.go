// Command slow-origin is the benchmark origin: an HTTP server with configurable
// latency and jitter, body size, Cache-Control and keyspace (ORIGIN_ADDR,
// ORIGIN_LATENCY, ORIGIN_JITTER, ORIGIN_BODY_BYTES, ORIGIN_CACHE_CONTROL,
// ORIGIN_KEYSPACE) serving GET /obj/{n}.
//
// STUB written by the integrator to freeze the contract; workstream C owns this
// command and replaces the body, keeping the env contract below.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
	fmt.Printf("stub: slow-origin not implemented yet (addr=%s latency=%s jitter=%s body=%d cc=%q keyspace=%d)\n",
		opts.addr, opts.latency, opts.jitter, opts.bodyBytes, opts.cacheControl, opts.keyspace)
	return nil
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
	if o.keyspace < 1 {
		errs = append(errs, errors.New("ORIGIN_KEYSPACE must be >= 1"))
	}
	if o.bodyBytes < 0 {
		errs = append(errs, errors.New("ORIGIN_BODY_BYTES must be >= 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return options{}, err
	}
	return o, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
