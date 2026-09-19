// Package metrics is a hand-written Prometheus text registry (no client_golang,
// aigw/sysmon style) exposing the edgecache_* families with fixed-enum labels
// only, plus a JSON snapshot for /admin/stats. It satisfies cache.Recorder.
//
// STUB written by the integrator to freeze the contract; workstream D owns this
// package and replaces the bodies, keeping every exported signature.
package metrics

import (
	"net/http"
	"time"

	"github.com/crowbett/edgecache/internal/store"
)

// ContentType is the Prometheus text exposition media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Registry holds every counter, gauge and histogram.
type Registry struct {
	build map[string]string
}

// NewRegistry creates a registry whose edgecache_build_info carries the given
// labels (version, go_version, ...).
func NewRegistry(build map[string]string) *Registry {
	return &Registry{build: build}
}

// Request records one client request: outcome label, status, time to first
// byte, total duration and bytes written.
func (m *Registry) Request(outcome string, status int, ttfb, total time.Duration, bytes int64) {
	_, _, _, _, _, _ = m, outcome, status, ttfb, total, bytes
}

// Origin records one upstream exchange; err non-nil counts as status 0.
func (m *Registry) Origin(status int, d time.Duration, err error) {
	_, _, _, _ = m, status, d, err
}

// Collapsed adds waiters to edgecache_collapsed_total.
func (m *Registry) Collapsed(waiters int) {
	_, _ = m, waiters
}

// StoreGauges refreshes the per-tier bytes/entries/evictions gauges.
func (m *Registry) StoreGauges(stats map[string]store.Stats) {
	_, _ = m, stats
}

// Purged adds n to edgecache_purges_total{kind}.
func (m *Registry) Purged(kind string, n int) {
	_, _, _ = m, kind, n
}

// VariantEvicted increments edgecache_variant_evictions_total.
func (m *Registry) VariantEvicted() {
	_ = m
}

// Handler serves the text exposition (text/plain; version=0.0.4).
func (m *Registry) Handler() http.Handler {
	_ = m
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
	})
}

// SnapshotJSON renders the current values as JSON for /admin/stats.
func (m *Registry) SnapshotJSON() []byte {
	_ = m
	return []byte("{}")
}
