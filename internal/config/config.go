// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the service reads at startup.
type Config struct {
	ServiceName     string
	Env             string // dev | staging | prod
	Addr            string
	LogLevel        slog.Level
	LogFormat       string // json | text
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration

	// OTELEnabled turns on OTLP trace + metric export (OTEL_ENABLED=true).
	OTELEnabled bool
	// OTELEndpoint is the OTLP/gRPC collector URL (OTEL_EXPORTER_OTLP_ENDPOINT).
	OTELEndpoint string
}

// Load reads configuration from the environment, applying sane defaults.
func Load() (Config, error) {
	cfg := Config{
		ServiceName:     getenv("SERVICE_NAME", "api"),
		Env:             getenv("APP_ENV", "dev"),
		Addr:            getenv("HTTP_ADDR", ":8080"),
		LogFormat:       strings.ToLower(getenv("LOG_FORMAT", "json")),
		RequestTimeout:  30 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		OTELEndpoint:    getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}

	var err error
	if cfg.OTELEnabled, err = getBool("OTEL_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = getDuration("REQUEST_TIMEOUT", cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = getDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}

	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return Config{}, fmt.Errorf("LOG_FORMAT must be json or text, got %q", cfg.LogFormat)
	}

	return cfg, nil
}

// IsProd reports whether the service is running in production mode.
func (c Config) IsProd() bool { return c.Env == "prod" }

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	// Accept both "30s" and a bare number of seconds.
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
