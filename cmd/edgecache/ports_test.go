package main

import (
	"testing"

	"github.com/crowbett/edgecache/internal/cache"
	"github.com/crowbett/edgecache/internal/diskstore"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/metrics"
	"github.com/crowbett/edgecache/internal/origin"
	"github.com/crowbett/edgecache/internal/server"
	"github.com/crowbett/edgecache/internal/tiered"
)

// The ports are consumer-defined; this is the one place that pins which
// concrete types satisfy them, so a lane breaking a contract fails here first.
var (
	_ cache.Store    = (*memstore.Store)(nil)
	_ cache.Store    = (*diskstore.Store)(nil)
	_ cache.Store    = (*tiered.Store)(nil)
	_ cache.Walker   = (*diskstore.Store)(nil)
	_ cache.Walker   = (*tiered.Store)(nil)
	_ cache.Sweeper  = (*diskstore.Store)(nil)
	_ cache.Sweeper  = (*tiered.Store)(nil)
	_ tiered.Tier    = (*memstore.Store)(nil)
	_ tiered.Tier    = (*diskstore.Store)(nil)
	_ cache.Fetcher  = (*origin.Origin)(nil)
	_ cache.Recorder = (*metrics.Registry)(nil)
	_ server.Purger  = (*cache.Cache)(nil)
)

func TestPortsSatisfied(t *testing.T) {
	t.Parallel()
	// Compile-time assertions above are the test; this keeps the file a real test file.
}
