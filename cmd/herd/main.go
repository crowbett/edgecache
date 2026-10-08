// Command herd measures a thundering herd on cold keys: every round it
// releases -n concurrent GETs for one URL nobody has fetched yet
// (<-base><round+start>), records each request's time to first body byte and
// total time, and splits them by role: the collapse leader (X-Cache: MISS) and
// the waiters that joined it. It prints a table and, with -json, writes the
// raw per-round numbers and the summary.
//
//	herd -base http://127.0.0.1:8081/obj/ -n 64 -rounds 10 -start 100 -bytes 8388608
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

type options struct {
	base    string
	n       int
	rounds  int
	start   int
	bytes   int64
	timeout time.Duration
	label   string
	jsonOut string
}

// sample is one request's outcome.
type sample struct {
	Round  int     `json:"round"`
	Role   string  `json:"role"` // leader | waiter | other
	XCache string  `json:"x_cache"`
	TTFBms float64 `json:"ttfb_ms"`
	Totms  float64 `json:"total_ms"`
	Bytes  int64   `json:"bytes"`
	Err    string  `json:"err,omitempty"`
}

// roleStats summarises one role over every round.
type roleStats struct {
	Role    string  `json:"role"`
	Count   int     `json:"count"`
	Errors  int     `json:"errors"`
	TTFBp50 float64 `json:"ttfb_p50_ms"`
	TTFBp99 float64 `json:"ttfb_p99_ms"`
	Totp50  float64 `json:"total_p50_ms"`
	Totp99  float64 `json:"total_p99_ms"`
}

type report struct {
	Label   string      `json:"label"`
	Base    string      `json:"base"`
	N       int         `json:"n"`
	Rounds  int         `json:"rounds"`
	Bytes   int64       `json:"bytes"`
	Stats   []roleStats `json:"stats"`
	Samples []sample    `json:"samples"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "herd:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.base, "base", "http://127.0.0.1:8081/obj/", "URL prefix; the round's key number is appended")
	flag.IntVar(&o.n, "n", 64, "concurrent requests per round")
	flag.IntVar(&o.rounds, "rounds", 10, "rounds, one cold key each")
	flag.IntVar(&o.start, "start", 100, "first key number")
	flag.Int64Var(&o.bytes, "bytes", -1, "expected body bytes (-1: do not check)")
	flag.DurationVar(&o.timeout, "timeout", 2*time.Minute, "per-request budget")
	flag.StringVar(&o.label, "label", "", "free-form label copied into the report")
	flag.StringVar(&o.jsonOut, "json", "", "write the report as JSON to this file")
	flag.Parse()
	if o.n < 1 || o.rounds < 1 {
		return errors.New("-n and -rounds must be >= 1")
	}

	client := &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: o.n,
		MaxConnsPerHost:     0,
		DisableCompression:  true,
	}}
	ctx := context.Background()
	// Open o.n connections first so dial time is not part of round 1.
	if err := warm(ctx, client, o); err != nil {
		return err
	}

	var samples []sample
	for round := range o.rounds {
		u := o.base + strconv.Itoa(o.start+round)
		samples = append(samples, herd(ctx, client, u, round, o)...)
	}
	rep := report{Label: o.label, Base: o.base, N: o.n, Rounds: o.rounds, Bytes: o.bytes, Samples: samples}
	for _, role := range []string{"leader", "waiter", "other"} {
		if st, ok := summarise(samples, role); ok {
			rep.Stats = append(rep.Stats, st)
		}
	}
	printTable(rep)
	if o.jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		if err := os.WriteFile(o.jsonOut, b, 0o600); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	for _, st := range rep.Stats {
		if st.Errors > 0 {
			return fmt.Errorf("%d %s request(s) failed", st.Errors, st.Role)
		}
	}
	return nil
}

// warm fetches a key outside the measured range on o.n parallel connections.
func warm(ctx context.Context, client *http.Client, o options) error {
	u := o.base + strconv.Itoa(o.start+o.rounds+1)
	for _, s := range herd(ctx, client, u, -1, o) {
		if s.Err != "" {
			return fmt.Errorf("warm-up %s: %s", u, s.Err)
		}
	}
	return nil
}

// herd releases o.n GETs for u at once and returns their samples.
func herd(ctx context.Context, client *http.Client, u string, round int, o options) []sample {
	out := make([]sample, o.n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range out {
		wg.Go(func() {
			<-gate
			out[i] = fetch(ctx, client, u, round, o)
		})
	}
	close(gate)
	wg.Wait()
	return out
}

func fetch(ctx context.Context, client *http.Client, u string, round int, o options) sample {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s := sample{Round: round, Role: "other"}
	var first time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Now() }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, u, http.NoBody)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	defer resp.Body.Close()
	s.XCache = resp.Header.Get("X-Cache")
	switch s.XCache {
	case "MISS":
		s.Role = "leader"
	case "HIT":
		s.Role = "waiter"
	}
	s.Bytes, err = io.Copy(io.Discard, resp.Body)
	end := time.Now()
	s.TTFBms = ms(first.Sub(start))
	s.Totms = ms(end.Sub(start))
	switch {
	case err != nil:
		s.Err = err.Error()
	case resp.StatusCode != http.StatusOK:
		s.Err = "status " + resp.Status
	case o.bytes >= 0 && s.Bytes != o.bytes:
		s.Err = fmt.Sprintf("body %d bytes, want %d", s.Bytes, o.bytes)
	}
	return s
}

func summarise(samples []sample, role string) (roleStats, bool) {
	st := roleStats{Role: role}
	var ttfb, tot []float64
	for _, s := range samples {
		if s.Role != role {
			continue
		}
		st.Count++
		if s.Err != "" {
			st.Errors++
			continue
		}
		ttfb, tot = append(ttfb, s.TTFBms), append(tot, s.Totms)
	}
	if st.Count == 0 {
		return st, false
	}
	st.TTFBp50, st.TTFBp99 = pct(ttfb, 50), pct(ttfb, 99)
	st.Totp50, st.Totp99 = pct(tot, 50), pct(tot, 99)
	return st, true
}

// pct is the nearest-rank percentile of v (NaN-free: 0 for an empty slice).
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	v = slices.Clone(v)
	slices.Sort(v)
	i := int(float64(len(v))*p/100+0.5) - 1
	return v[min(max(i, 0), len(v)-1)]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func printTable(rep report) {
	fmt.Printf("herd %s: n=%d rounds=%d base=%s\n", rep.Label, rep.N, rep.Rounds, rep.Base)
	fmt.Println("| role | requests | errors | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|")
	for _, st := range rep.Stats {
		fmt.Printf("| %s | %d | %d | %.1f | %.1f | %.1f | %.1f |\n",
			st.Role, st.Count, st.Errors, st.TTFBp50, st.TTFBp99, st.Totp50, st.Totp99)
	}
}
