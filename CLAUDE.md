# edgecache

RFC 9111 HTTP caching reverse proxy in Go: freshness/validation, stale-while-revalidate / stale-if-error, Vary, singleflight collapse with a detached leader, memory + disk tiers, purge API, hand-written Prometheus text. Caching packages are standard library only; OpenTelemetry (`internal/otel`, `internal/http`) is the one third-party dependency, off by default. Module `github.com/crowbett/edgecache`, Go 1.27. Global Go conventions from `~/.claude/CLAUDE.md` apply.

## Where things are
- `docs/PLAN.md`: design record (port signatures, config table, data flow, teardown, frozen types); read before changing a contract.
- `docs/PACKAGES.md`: what every package does. `docs/CONVENTIONS.md`: full text of the rules below.

## Invariants
- **Three layers.** `internal/rfc9111` is pure policy: no I/O, no goroutines, no clock (`now time.Time` is a parameter; nothing in it or its tests calls `time.Now()`). `internal/store` + `memstore`/`diskstore`/`tiered` are dumb blob storage and never read a header. `internal/cache` is the engine; `internal/server` mounts it.
- Imports flow one way: `server -> cache -> {rfc9111, cachekey, store, origin, collapse, tagindex, metrics}`; `tiered -> {store, memstore, diskstore}`; `memstore|diskstore -> store`; `store -> rfc9111`; `rfc9111`, `cachekey` -> stdlib only. No cycles, no `util`/`common`/`models`.
- Frozen types (changed only by the integrator, with `docs/PLAN.md`): `store.Meta`, `store.Writer`, `cache.Store`, `cache.Fetcher`, `cache.Recorder`, `rfc9111.Resp`, `rfc9111.Decision`, `config.Config`. Other step-1 signatures: replace bodies, keep signatures.
- Ports are consumer-defined; satisfaction is asserted in `cmd/edgecache/ports_test.go`.
- The proxy handler is mounted WITHOUT `Timeout`/`http.TimeoutHandler`; its budget is the origin context. Only the admin listener gets the full template stack.
- A miss streams; a client write error does not abort the fetch (up to `ORPHAN_MAX_BYTES`). The collapse leader runs on a root-derived context, never the caller's.
- Metric labels are fixed enums only (never a URL or client header). `Warning` is never emitted. Every RFC deviation is a named config key defaulting to spec-compliant.
- Admin-listener errors go through `writeError` (stable strings); internals are logged with the request id, never returned.
- `.golangci.yml` is v2; hooks format on save. Every exported identifier is documented. Don't fix an "unused" stub with `//nolint`.
- Ctrl-Break testing on Windows: start edgecache in its own console (`Start-Process`) first, or the group-wide event kills the load generator and slow-origin too.
- Every number in README.md is pasted from a real run on this box and names the machine, settings and the `results/<ts>/summary.md` or `.bench/*.txt` it came from. Never estimate or carry a number forward after hot-path code changed. A PGO profile is committed only when `pgo.ps1` shows no regression in any round.

## Commands (Taskfile.yml)
- `task origin` (slow-origin :8082) | `task run` (edgecache :8081, admin :9101) | `task build`
- `task test` (`-race`) / `task test:fast` | `task vet` | `task lint` | `task fmt` | `task check`
- `task load` | `task pgo` | `task bench` | `task bench:compare` (scripts need `pwsh`, `oha`, free ports 8081/8082/9101)

## Definition of done
`go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run ./...` all green (say which you ran). For any perf change also a fresh `scripts/cache.ps1` table in `results/<ts>/summary.md` and `scripts/bench.ps1 -Compare baseline`.
