# edgecache

Minimal, production-shaped Go HTTP API. Standard library (`net/http` 1.22+ method
patterns, `log/slog`) plus optional OpenTelemetry; no framework.

```
cmd/edgecache/main.go            entrypoint: config -> logger -> otel -> server -> graceful shutdown
internal/config/           env-driven config (HTTP_ADDR, LOG_LEVEL, LOG_FORMAT, REQUEST_TIMEOUT, OTEL_*, ...)
internal/http/             router, middleware chain (request-id, tracing, logging, recover, timeout), handlers, tests
internal/otel/             OTel setup (OTLP/gRPC traces + metrics, runtime metrics) and slog trace-id handler
Taskfile.yml               task build|run|dev|test|lint|cover|docker:build|up
Dockerfile                 multi-stage -> distroless static, non-root
compose.yaml               api + postgres:17 + redis:7 with healthchecks
.golangci.yml              golangci-lint v2 config (gofumpt + goimports formatters)
.air.toml                  hot reload
.github/workflows/ci.yml   test (race, linux+windows), lint, govulncheck, docker build
```

## Quick start

```powershell
go run ./cmd/edgecache                       # http://localhost:8080
curl localhost:8080/healthz
curl localhost:8080/readyz
curl localhost:8080/api/v1/ping
```

With [Task](https://taskfile.dev):

```powershell
task run        # go run
task dev        # air hot reload
task test:fast  # go test (no -race; -race needs a C compiler on Windows)
task lint       # golangci-lint
task up         # docker compose up -d --build --wait
```

## Configuration

| Var                | Default | Notes                         |
| ------------------ | ------- | ----------------------------- |
| `HTTP_ADDR`        | `:8080` | listen address                |
| `APP_ENV`          | `dev`   | dev / staging / prod          |
| `LOG_LEVEL`        | `info`  | debug / info / warn / error   |
| `LOG_FORMAT`       | `json`  | json / text                   |
| `REQUEST_TIMEOUT`  | `30s`   | per-request handler timeout   |
| `SHUTDOWN_TIMEOUT` | `10s`   | graceful drain window         |
| `OTEL_ENABLED`     | `false` | push traces + metrics over OTLP/gRPC |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | collector URL (http:// or bare host:port = plaintext) |

## Endpoints

- `GET /healthz` - liveness (always 200 while the process runs)
- `GET /readyz` - readiness (flip with `Readiness.Set(false)` while draining)
- `GET /api/v1/ping` - `{"message":"pong","request_id":"...","time":"..."}`

## Adding a route

1. Add a handler in `internal/http/handlers.go`.
2. Register it in `registerRoutes` (the single route table) with a method pattern: `mux.HandleFunc("POST /api/v1/things", handleCreateThing)`.
3. Add a table-driven case in `handlers_test.go`.

## Observability

Off by default. With `OTEL_ENABLED=true` the service pushes to an OTLP/gRPC collector:

- **Traces**: one server span per request via `otelhttp`, named after the mux pattern
  (`GET /api/v1/ping`), with `http.route`, status and the `X-Request-Id` as `http.request.id`.
  W3C `traceparent` is propagated in and out. `/healthz` and `/readyz` are filtered out.
- **Metrics**: `http.server.request.duration` (seconds histogram, labelled by route/method/
  status) and Go runtime metrics (`go.goroutine.count`, `go.memory.used`, `go.memory.gc.goal`,
  `go.processor.limit`, ...), exported every 15s (`OTEL_METRIC_EXPORT_INTERVAL` overrides).
- **Logs** stay on stdout in the existing slog format; `internal/otel.LogHandler` appends
  `trace_id` / `span_id` to any record logged with a request context, so Loki -> Tempo links
  work once container logs are shipped. (The official `otelslog` bridge is still v0.x, hence
  this 40-line wrapper instead.)
- Resource: `service.name` (= `SERVICE_NAME`), `service.version` (ldflags), `deployment.environment.name`
  (= `APP_ENV`), host + SDK info. `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` override.

Shutdown order is server drain -> OTel flush (own 5s timeout), so the last spans get out.

Wiring: `internal/otel.Setup` installs the global providers; `internal/http.Tracing` is the
otelhttp middleware and `TagRoute` (innermost) copies the mux pattern up to the span and
metric labeler because `http.TimeoutHandler` hands the mux a copy of the request.

Local backends (Grafana on http://localhost:3000, dashboard "Go service (OTel)"):

```powershell
cd ..\docker-stack; docker compose --profile obs up -d --wait; cd ..\api
$env:OTEL_ENABLED = "true"; go run ./cmd/edgecache            # collector on localhost:4317
# or in a container (reaches the collector through host.docker.internal:4317):
$env:OTEL_ENABLED = "true"; docker compose up -d --build --wait
```

If the collector is down the SDK logs `otel sdk error` warnings and keeps serving.

## Docker notes

The runtime image is `gcr.io/distroless/static-debian12:nonroot`: no shell, no `curl`,
so there is no `HEALTHCHECK` instruction. Probe `GET /healthz` from your orchestrator.

## Tooling

```powershell
go install github.com/go-task/task/v3/cmd/task@latest
go install github.com/air-verse/air@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
go install mvdan.cc/gofumpt@latest
```
