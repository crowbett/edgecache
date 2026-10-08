// Command edgecache is an RFC 9111 HTTP caching reverse proxy: freshness and
// validation, stale-while-revalidate / stale-if-error, Vary, singleflight
// collapse with a detached leader, memory + disk tiers, a purge API and
// hand-written Prometheus text.
//
// Startup: config -> logger -> OTel (off by default) -> stores (memory, disk
// when EDGECACHE_DISK_DIR is set, tiered) -> origin client -> metrics registry
// -> cache engine -> two listeners (proxy on EDGECACHE_ADDR, admin on the
// loopback EDGECACHE_ADMIN_ADDR) -> janitor. Teardown is staged; see teardown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	"github.com/crowbett/edgecache/internal/config"
	"github.com/crowbett/edgecache/internal/diskstore"
	apihttp "github.com/crowbett/edgecache/internal/http"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/metrics"
	"github.com/crowbett/edgecache/internal/origin"
	"github.com/crowbett/edgecache/internal/otel"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/server"
	"github.com/crowbett/edgecache/internal/tiered"
	"github.com/crowbett/edgecache/internal/trustproxy"
)

// Budgets for the teardown stages that are not user-configurable (stage 2 is
// SHUTDOWN_TIMEOUT).
const (
	adminStopTimeout = 5 * time.Second // stage 3: admin listener drain
	drainTimeout     = 5 * time.Second // stage 4: background refreshes + collapse leaders
	janitorStopWait  = 2 * time.Second // stage 4: wait for cache.Run to return
	otelFlushTimeout = 5 * time.Second // stage 6: final span/metric export
	stages           = 6
)

// version is injected at build time: -ldflags="-X main.version=v1.2.3".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- Observability (no-op unless OTEL_ENABLED=true) ------------------------
	otelStop, err := otel.Setup(ctx, otel.Config{
		Enabled: cfg.OTELEnabled, ServiceName: cfg.ServiceName, ServiceVersion: version,
		Environment: cfg.Env, Endpoint: cfg.OTELEndpoint, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() { // stage 6/6
		flushCtx, cancel := context.WithTimeout(context.Background(), otelFlushTimeout)
		defer cancel()
		if err := otelStop(flushCtx); err != nil {
			logger.Warn("teardown 6/6: otel flush", slog.Any("err", err))
		} else {
			logger.Info("teardown 6/6: otel flushed")
		}
	}()

	// --- Store, origin, metrics, engine -----------------------------------------
	st, err := newStore(cfg, logger)
	if err != nil {
		return err
	}
	closed := false
	defer func() { // stage 5/6 on the early-error paths; teardown does it otherwise
		if !closed {
			_ = st.Close()
		}
	}()

	trusted := trustproxy.New(cfg.TrustedProxies)
	org, err := origin.New(origin.Config{
		Base:            cfg.Origin,
		HostOverride:    cfg.OriginHost,
		HeaderTimeout:   cfg.OriginHeaderTimeout,
		Total:           cfg.OriginTimeout,
		MaxConnsPerHost: cfg.OriginMaxConns,
		TrustedProxies:  trusted,
	}, logger)
	if err != nil {
		return fmt.Errorf("origin: %w", err)
	}

	reg := metrics.NewRegistry(map[string]string{"version": version})
	reg.SetStoreStatsFunc(st.Stats)

	eng, err := cache.New(cache.Config{
		Policy: rfc9111.Policy{
			HeuristicFraction:  cfg.HeuristicFraction,
			HeuristicMax:       cfg.HeuristicMax,
			CacheAuthorization: cfg.CacheAuthorization,
		},
		MaxObjectBytes:      cfg.MaxObjectBytes,
		MaxVariants:         cfg.MaxVariants,
		DefaultSWR:          cfg.DefaultSWR,
		DefaultSIE:          cfg.DefaultSIE,
		OrphanFetch:         cfg.OrphanFetch,
		OrphanMaxBytes:      cfg.OrphanMaxBytes,
		TagHeaders:          cfg.TagHeaders,
		JanitorInterval:     cfg.JanitorInterval,
		CollapseLeadTimeout: cfg.CollapseLeadTimeout,
		CollapseStreamBytes: cfg.CollapseStreamBytes,
		PurgeVerbose:        cfg.PurgeVerbose,
		TrustedProxies:      trusted,
		RangeFill:           cfg.RangeFill,
		Debug:               cfg.LogLevel <= slog.LevelDebug, // X-Cache-Key on responses
	}, st, org, reg, logger)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}

	// --- Listeners --------------------------------------------------------------
	ready := &apihttp.Readiness{}
	ready.Set(true)
	srvCfg := server.Config{
		ServiceName:     cfg.ServiceName,
		Addr:            cfg.Addr,
		AdminAddr:       cfg.AdminAddr,
		PurgeToken:      cfg.PurgeToken,
		RequestTimeout:  cfg.RequestTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout,
		TrustedProxies:  trusted,
	}
	proxySrv, adminSrv := server.NewServers(srvCfg, server.Deps{
		Proxy:         eng.Handler(),
		Purger:        eng,
		Metrics:       reg.Handler(),
		Stats:         reg.SnapshotJSON,
		Ready:         ready,
		Log:           logger,
		Version:       version,
		StoreStats:    st.Stats,
		InFlight:      eng.InFlight,
		TagIndexLen:   eng.TagIndexLen,
		ConfigSummary: configSummary(cfg),
	})

	// Bind both ports before anything serves so a clash fails fast and clean.
	lc := &net.ListenConfig{}
	proxyLis, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen proxy %s: %w", cfg.Addr, err)
	}
	adminLis, err := lc.Listen(ctx, "tcp", cfg.AdminAddr)
	if err != nil {
		_ = proxyLis.Close()
		return fmt.Errorf("listen admin %s: %w", cfg.AdminAddr, err)
	}

	// Janitor: sweeps tombstones and refreshes gauges on its own root; when that
	// root ends Run returns and cancels every leader and background refresh.
	janitorCtx, stopJanitor := context.WithCancel(context.WithoutCancel(ctx))
	defer stopJanitor()
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		eng.Run(janitorCtx)
	}()

	errCh := make(chan error, 2)
	serve := func(name string, srv *http.Server, lis net.Listener) {
		logger.Info(name+" listening", slog.String("addr", lis.Addr().String()))
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s serve: %w", name, err)
		}
	}
	logger.Info("edgecache starting",
		slog.String("version", version),
		slog.String("origin", cfg.Origin.Redacted()),
		slog.String("env", cfg.Env),
		slog.Int64("mem_bytes", cfg.MemBytes),
		slog.String("disk_dir", cfg.DiskDir),
		slog.Bool("purge_enabled", cfg.PurgeEnabled()),
	)
	go serve("proxy", proxySrv, proxyLis)
	go serve("admin", adminSrv, adminLis)

	select {
	case <-ctx.Done():
		logger.Info("signal received; starting staged teardown", slog.Int("stages", stages))
	case err := <-errCh:
		logger.Error("listener failed; starting staged teardown", slog.Any("err", err))
		teardown(cfg, logger, proxySrv, adminSrv, ready, eng, org, st, stopJanitor, janitorDone)
		closed = true
		return err
	}

	teardown(cfg, logger, proxySrv, adminSrv, ready, eng, org, st, stopJanitor, janitorDone)
	closed = true
	logger.Info("edgecache stopped")
	return nil
}

// teardown runs stages 1-5 with independent budgets (stage 6, the OTel flush,
// is deferred in run):
//  1. readyz -> 503 so a balancer stops sending.
//  2. drain the proxy listener for SHUTDOWN_TIMEOUT; on timeout cancel the
//     engine root (every collapse leader and orphan fetch aborts) and Close.
//  3. drain the admin listener (5 s).
//  4. cancel the engine root, wait for background refreshes and leaders (5 s),
//     close idle origin connections.
//  5. close the store: memory drops, disk flushes metas and releases tombstones.
func teardown(cfg config.Config, logger *slog.Logger, proxySrv, adminSrv *http.Server, ready *apihttp.Readiness,
	eng *cache.Cache, org *origin.Origin, st cache.Store, stopJanitor context.CancelFunc, janitorDone <-chan struct{},
) {
	ready.Set(false)
	logger.Info("teardown 1/6: readiness off")

	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	if err := proxySrv.Shutdown(drainCtx); err != nil {
		logger.Warn("teardown 2/6: proxy drain timed out; cancelling in-flight fetches", slog.Any("err", err))
		eng.Stop()
		_ = proxySrv.Close()
	} else {
		logger.Info("teardown 2/6: proxy drained")
	}
	cancel()

	adminCtx, cancel := context.WithTimeout(context.Background(), adminStopTimeout)
	if err := adminSrv.Shutdown(adminCtx); err != nil {
		logger.Warn("teardown 3/6: admin drain timed out", slog.Any("err", err))
		_ = adminSrv.Close()
	} else {
		logger.Info("teardown 3/6: admin drained")
	}
	cancel()

	stopJanitor()
	select {
	case <-janitorDone:
	case <-time.After(janitorStopWait):
		logger.Warn("teardown 4/6: janitor did not stop in time")
	}
	eng.Stop()
	bgCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	if err := eng.Drain(bgCtx); err != nil {
		logger.Warn("teardown 4/6: background work did not finish; cancelled", slog.Any("err", err))
	} else {
		logger.Info("teardown 4/6: background refreshes and leaders finished")
	}
	cancel()
	org.CloseIdleConnections()

	if err := st.Close(); err != nil {
		logger.Warn("teardown 5/6: store close", slog.Any("err", err))
	} else {
		logger.Info("teardown 5/6: store closed")
	}
}

// newStore builds the memory tier, the disk tier when EDGECACHE_DISK_DIR is
// set, and composes them; a nil disk tier makes tiered a pure-memory store.
func newStore(cfg config.Config, logger *slog.Logger) (*tiered.Store, error) {
	mem, err := memstore.New(memstore.Config{
		Capacity:       cfg.MemBytes,
		Shards:         cfg.MemShards,
		MaxObjectBytes: cfg.MemMaxObjectBytes,
		MaxVariants:    cfg.MaxVariants,
	})
	if err != nil {
		return nil, fmt.Errorf("memstore: %w", err)
	}
	tcfg := tiered.Config{MemMaxObjectBytes: cfg.MemMaxObjectBytes}
	if !cfg.DiskEnabled() {
		return tiered.New(tcfg, mem, nil), nil
	}
	disk, err := diskstore.New(diskstore.Config{
		Dir:            cfg.DiskDir,
		Capacity:       cfg.DiskBytes,
		MaxObjectBytes: cfg.MaxObjectBytes,
		MaxVariants:    cfg.MaxVariants,
	}, logger)
	if err != nil {
		_ = mem.Close()
		return nil, fmt.Errorf("diskstore %s: %w", cfg.DiskDir, err)
	}
	c := disk.Counters()
	logger.Info("disk tier recovered",
		slog.String("dir", cfg.DiskDir),
		slog.Int64("orphans_swept", c.Orphans),
		slog.Int64("corrupt_metas", c.Corrupt),
	)
	return tiered.New(tcfg, mem, disk), nil
}

// configSummary is the non-secret config subset /admin/stats shows.
func configSummary(cfg config.Config) map[string]any {
	return map[string]any{
		"origin":                cfg.Origin.Redacted(),
		"origin_host":           cfg.OriginHost,
		"origin_header_timeout": cfg.OriginHeaderTimeout.String(),
		"origin_timeout":        cfg.OriginTimeout.String(),
		"origin_max_conns":      cfg.OriginMaxConns,
		"mem_bytes":             cfg.MemBytes,
		"mem_shards":            cfg.MemShards,
		"mem_max_object_bytes":  cfg.MemMaxObjectBytes,
		"disk_enabled":          cfg.DiskEnabled(),
		"disk_dir":              cfg.DiskDir,
		"disk_bytes":            cfg.DiskBytes,
		"max_object_bytes":      cfg.MaxObjectBytes,
		"max_variants":          cfg.MaxVariants,
		"heuristic_fraction":    cfg.HeuristicFraction,
		"heuristic_max":         cfg.HeuristicMax.String(),
		"default_swr":           cfg.DefaultSWR.String(),
		"default_sie":           cfg.DefaultSIE.String(),
		"cache_authorization":   cfg.CacheAuthorization,
		"orphan_fetch":          cfg.OrphanFetch,
		"orphan_max_bytes":      cfg.OrphanMaxBytes,
		"purge_verbose":         cfg.PurgeVerbose,
		"tag_headers":           cfg.TagHeaders,
		"janitor_interval":      cfg.JanitorInterval.String(),
		"collapse_lead_timeout": cfg.CollapseLeadTimeout.String(),
		"collapse_stream_bytes": cfg.CollapseStreamBytes,
		"range_fill":            cfg.RangeFill,
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	// Stamps trace_id/span_id on records logged with a span in context (no-op otherwise).
	return slog.New(otel.NewLogHandler(h)).With("service", cfg.ServiceName)
}
