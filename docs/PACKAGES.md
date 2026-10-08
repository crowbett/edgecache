# Package layout

Moved verbatim from CLAUDE.md. See [../CLAUDE.md](../CLAUDE.md) for the short invariant list.

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

