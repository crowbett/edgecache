# edgecache

RFC 9111 HTTP caching reverse proxy in Go: freshness/validation, stale-while-revalidate / stale-if-error,
Vary, singleflight collapse with a detached leader, memory + disk tiers, purge API, hand-written Prometheus
text. Standard library only (`net/http` 1.22 patterns, `log/slog`, `crypto/sha256`, `container/list`); the
template's OTel stack stays, off by default. Module `github.com/crowbett/edgecache`, Go 1.27.
Global Go conventions from `~/.claude/CLAUDE.md` apply. The design record (every port signature, the config
table, data flow, teardown stages, fanout and frozen types) is `docs/PLAN.md`; read it before changing a contract.

## Layout
- `cmd/edgecache/main.go` - wiring only: config -> logger -> otel -> store -> origin -> cache -> two servers ->
  staged teardown. `cmd/edgecache/ports_test.go` pins which concrete types satisfy the consumer-defined ports.
- `cmd/slow-origin/main.go` - benchmark origin (`ORIGIN_ADDR/_LATENCY/_JITTER/_BODY_BYTES/_CACHE_CONTROL/_KEYSPACE`).
- `internal/config/` - env -> `config.Config`, joined validation at startup (`Validate` probes the disk dir).
- `internal/http/` - template middleware chain (`Chain`, `RequestID`, `Tracing`, `Logging`, `Recover`, `Timeout`,
  `TagRoute`, `Readiness`), unchanged. `internal/otel/` - OTel bootstrap.
- `internal/cachekey/` - `Key [32]byte`, `Canonical`, `Primary`, `Secondary`, `NormalizeVary`. stdlib only.
- `internal/rfc9111/` - pure policy: directives, `Storable`, `Lifetime`, `Age`, `Evaluate`, `ConditionalRequest`,
  `Merge304`, `StripHopByHop`. stdlib only.
- `internal/store/` - port types (`Meta`, `Entry`, `Variant`, `Writer`, `Stats`, sentinels). No I/O.
- `internal/memstore/` (sharded LRU, byte budget), `internal/diskstore/` (content-addressed files, temp+rename,
  meta last, scan recovery, refcounts + tombstones), `internal/tiered/` (memory in front of disk).
- `internal/collapse/` - generic `Group[T]` singleflight, leader on a root-derived context, `TryStart`.
- `internal/origin/` - upstream client: base URL, Host override, hop-by-hop strip, `Via`, retry-once.
- `internal/tagindex/` - surrogate keys + URL prefixes -> keys, for purge.
- `internal/cache/` - the engine: `Store`/`Fetcher`/`Recorder` ports, `Handler`, hit/miss/revalidate/stale
  paths, tee copy with orphan fetch, `Purge`, `Run` (janitor), `Drain`.
- `internal/metrics/` - hand-written Prometheus text registry + JSON snapshot; satisfies `cache.Recorder`.
- `internal/server/` - route tables and the two `http.Server`s (proxy listener, loopback admin listener).
- `internal/bench/` - hot-path microbenchmarks; `scripts/` - `_lib.ps1`, `cache.ps1`, `pgo.ps1`, `bench.ps1`.

## The three-layer rule
1. `internal/rfc9111` is **pure policy**: headers and values in, decisions out. No I/O, no goroutines, no clock of
   its own - `now time.Time` is always a parameter. Every rule is a table test with a fixed clock; nothing in the
   package (or its tests) calls `time.Now()`.
2. `internal/store` + `memstore`/`diskstore`/`tiered` are **dumb keyed blob storage**: bytes, budgets, refcounts,
   atomic publish, per-tier stats. Zero HTTP semantics - they never read a header.
3. `internal/cache` is the **engine** joining them; `internal/server` mounts it. Imports flow one way:
   `server -> cache -> {rfc9111, cachekey, store, origin, collapse, tagindex, metrics}`; `tiered -> {store, memstore,
   diskstore}`; `memstore|diskstore -> store`; `store -> rfc9111`; `rfc9111`, `cachekey` -> stdlib only.
   No cycles, no `util`/`common`/`models`.

## Commands (Taskfile.yml)
- `task origin` (slow-origin on 127.0.0.1:8082), `task run` (edgecache on :8081, admin :9101; `EDGECACHE_ORIGIN`
  defaults to `http://127.0.0.1:8082`, shell env wins), `task build` (both binaries into `bin/`, version from git).
- `task test` (`go test -race -count=1 ./...`) / `task test:fast`, `task vet`, `task lint`, `task fmt`, `task check`.
- `task load` (`scripts/cache.ps1`: cold/warm/no-cache/origin -> `results/<ts>/summary.md`), `task pgo`
  (`scripts/pgo.ps1`: profile + A/B/A/B -> `results/pgo/<ts>/summary.md`), `task bench` (`scripts/bench.ps1 -Save
  baseline` -> `.bench/baseline.txt`), `task bench:compare` (benchstat against the baseline).
- The scripts need `pwsh`, `oha` on PATH and free ports 8081/8082/9101; they build their own `bin/*.exe`.
- Ctrl-Break testing on Windows: every process launched from one shell shares a console, and
  `GenerateConsoleCtrlEvent` hits the whole group. Start edgecache in its own console (`Start-Process`) before
  signalling it, or the load generator and slow-origin die with it.

## Conventions specific to this repo
- **Frozen types** (changed only by the integrator, together with `docs/PLAN.md`): `store.Meta`, `store.Writer`,
  `cache.Store`, `cache.Fetcher`, `cache.Recorder`, `rfc9111.Resp`, `rfc9111.Decision`, `config.Config`. A lane
  that needs a change files it in its report instead of editing in place. Every other exported signature in the
  step-1 stubs is the contract the lanes compile against: replace bodies, keep signatures, add freely.
- Ports are consumer-defined (`cache.Store`, `cache.Fetcher`, `cache.Recorder`, `server.Purger`, `tiered.Tier`);
  `cmd/edgecache/ports_test.go` is where satisfaction is asserted, not in the implementing package.
- The proxy handler is mounted **without** `Timeout`/`http.TimeoutHandler` (aigw's streaming lesson); its budget
  is the origin context. Only the admin listener goes through the full template stack.
- A miss streams: one loop writes to the store writer and the client; a client write error does not abort the
  fetch (up to `ORPHAN_MAX_BYTES`). The collapse leader runs on a root-derived context, never the caller's.
- Metric labels are fixed enums only (never a URL, never a client header). Responses carry
  `X-Cache: HIT|MISS|STALE|REVALIDATED|BYPASS`, `Age`, `X-Cache-Reason` on bypass, `X-Cache-Key` in debug builds.
- Every deviation from the RFC is a named config key defaulting to spec-compliant (`CACHE_AUTHORIZATION=false`,
  `DEFAULT_SWR=0s`). `Warning` is never emitted.
- Errors to clients on the admin listener go through `writeError` (stable strings); internals are logged with the
  request id, never returned in the body.
- `.golangci.yml` is v2 with gofumpt+goimports formatters; hooks format on save, don't run them by hand. Every
  exported identifier is documented (revive `exported`). Don't fix an "unused" stub with `//nolint`.

## Definition of done
`go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run ./...` all green (say which
you ran), plus - for any perf change - a fresh `scripts/cache.ps1` table (mode, rps, p50/p90/p99, hit-ratio from
`/metrics` deltas, origin-rps) in `results/<ts>/summary.md` and `scripts/bench.ps1 -Compare baseline`.

## Measured numbers only
Every number in `README.md` (rps, p99, hit ratio, PGO delta, ns/op, allocs) is pasted from a real run on this box
and names the machine, the run settings and the `results/<ts>/summary.md` or `.bench/*.txt` it came from. Never
estimate, round up, or carry a number forward after the code on the hot path changed: re-run the script, replace
the table, commit the summary alongside. A PGO profile is committed only when `pgo.ps1` shows no regression in any
round (a gain inside the round-to-round noise is kept but labelled as such in the README); a regression means the
profile is rejected, not tuned.
