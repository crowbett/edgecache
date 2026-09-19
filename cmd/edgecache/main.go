// Command api is the HTTP API entrypoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crowbett/edgecache/internal/config"
	apihttp "github.com/crowbett/edgecache/internal/http"
	"github.com/crowbett/edgecache/internal/otel"
)

// otelShutdownTimeout bounds the final span/metric flush after the server has drained.
const otelShutdownTimeout = 5 * time.Second

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

	// OTel is a no-op unless OTEL_ENABLED=true; shutdown flushes after the server drains.
	otelShutdown, err := otel.Setup(ctx, otel.Config{
		Enabled:        cfg.OTELEnabled,
		ServiceName:    cfg.ServiceName,
		ServiceVersion: version,
		Environment:    cfg.Env,
		Endpoint:       cfg.OTELEndpoint,
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer cancel()
		if err := otelShutdown(flushCtx); err != nil {
			logger.Warn("otel shutdown", slog.Any("err", err))
		}
	}()

	handler := apihttp.NewRouter(logger, cfg)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", cfg.Addr, "env", cfg.Env, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
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
