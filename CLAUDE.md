# edgecache

Go HTTP API on the standard library (`net/http` 1.22 method patterns, `log/slog`) plus OpenTelemetry. Module `github.com/crowbett/edgecache`.
Global Go conventions from `~/.claude/CLAUDE.md` apply; this file is only what is specific here.

## Layout
- `cmd/edgecache/main.go` - config -> logger -> `http.Server` -> `signal.NotifyContext` graceful shutdown.
- `internal/config/` - env-driven `config.Config` (`HTTP_ADDR`, `LOG_LEVEL`, `LOG_FORMAT`, `REQUEST_TIMEOUT`, ...).
- `internal/http/` - `handlers.go` (router + handlers + `writeJSON`/`writeError`), `middleware.go`
  (`Chain`, `RequestID`, `Tracing`, `Logging`, `Recover`, `Timeout`, `TagRoute`), `handlers_test.go` (table-driven `TestRoutes`).
- `internal/otel/` - OpenTelemetry bootstrap (OTLP gRPC traces + metrics, slog bridge); read it before touching tracing.
- `db/migrations/` (goose) and `db/queries/` (sqlc) appear once `/db bootstrap` runs; generated code
  lands in `internal/db/sqlc/` and is committed, never hand-edited.
- `compose.yaml` - api + postgres:17 + redis:7 with healthchecks. `Dockerfile` - distroless static, non-root.

## Commands (Taskfile.yml)
- `task run` / `task dev` (air hot reload) / `task build` (-> `bin/`)
- `task test` (`go test -race -count=1 ./...`, needs a C compiler) or `task test:fast` (no -race)
- `task vet`, `task lint` (`golangci-lint run ./...`), `task fmt`, `task vuln`, `task check` (tidy+vet+lint+test:fast)
- `task up` / `task down` - compose stack; `/docker-up` diagnoses a stuck stack.

## Conventions specific to this repo
- Routes are registered once, in `registerRoutes` (`internal/http/handlers.go`), as
  `mux.HandleFunc("METHOD /path", handler)`; the `NewRouter*` constructors all build on it and
  `wrap` applies the middleware stack (Recover sits inside Timeout, TagRoute innermost).
  Every new route gets a case in the `TestRoutes` table. `/add-handler` does the whole wiring.
- Handlers only decode, validate, call, encode. No DB access in `internal/http`; put it behind a
  small consumer-defined interface and inject it through the router constructor.
- Errors to clients go through `writeError` (stable strings). Internals are logged with the request id
  (`RequestIDFrom(ctx)`), never returned in the body.
- Readiness: flip `Readiness.Set(false)` while draining; `/healthz` is liveness only.
- DB: goose migrations + sqlc queries via `/db` (repo-bound) or `/go-migrate` (general). `DROP TABLE`
  in a Down migration is always `IF EXISTS`.
- `.golangci.yml` is v2 with gofumpt+goimports formatters; hooks format on save, don't run them by hand.

## Definition of done
`go build ./...`, `go vet ./...`, `go test -race -count=1 ./...` (or `task test:fast` if no C compiler,
say which), and `golangci-lint run ./...` all green. `/go-test` and `/go-lint` run these with fixes.
