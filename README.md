# edgecache

An RFC 9111 HTTP caching reverse proxy in Go, standard library only: freshness and validation,
`stale-while-revalidate` / `stale-if-error`, `Vary`, singleflight collapse with a detached leader,
memory + disk tiers, a purge API and hand-written Prometheus text. It sits in front of one origin
and turns a 50 ms upstream into sub-millisecond hits, with the numbers measured below.

## Architecture

Three layers with hard boundaries, two listeners:

```
client -> :8081 proxy listener (RequestID/Tracing/Logging/Recover, NO TimeoutHandler)
            |-- PURGE <path>  (X-Purge-Token)
            `-- cache.Handler
                  1. method + request directives     (rfc9111.ParseRequest)
                  2. primary key -> variants -> secondary key -> Store.Get   (cachekey, tiered)
                  3. rfc9111.Evaluate(now, stored, directives)
                        Fresh ------------------> HIT   (Age, stored headers, body from Entry.Open)
                        StaleWhileRevalidate ---> STALE + one background refresh per key
                        Revalidate -------------> conditional to origin; 304 -> REVALIDATED, 200 -> MISS
                        Bypass -----------------> BYPASS (X-Cache-Reason), plain copy
                  4. miss: collapse.Do(key) -> ONE leader on a root-derived ctx -> origin.Do
                        tee loop: store writer + client; client gone -> keep filling (orphan fetch)
                        Commit -> Variant + tag index; waiters re-Get after commit
loopback :9101 admin listener (full template stack incl. Timeout)
            /healthz /readyz /metrics /admin/stats /admin/purge /debug/pprof/*
```

- `internal/rfc9111` is **pure policy**: headers in, decisions out, `now` is always a parameter.
- `internal/store` + `memstore` / `diskstore` / `tiered` are **dumb keyed blob storage**: bytes, budgets,
  refcounts, atomic publish. They never read an HTTP header.
- `internal/cache` is the **engine** joining them; `internal/server` mounts it. Imports flow one way
  (`server -> cache -> {rfc9111, cachekey, store, origin, collapse, tagindex, metrics}`).

## RFC 9111 coverage

| Area | Implemented | v1 limits |
|---|---|---|
| Methods (§4) | GET/HEAD cached; HEAD served from a stored GET; POST/PUT/PATCH/DELETE invalidate the primary key on a non-error response | POST responses are never stored |
| Storable (§3) | status in {200,203,204,300,301,308,404,405,410,414,451}; `no-store` either side, `private`, `Vary: *` reject; §3.5 `Authorization` rule (`EDGECACHE_CACHE_AUTHORIZATION` overrides) | |
| Freshness (§4.2) | `s-maxage` > `max-age` > `Expires - Date` > heuristic (`0.1 x (Date - Last-Modified)`, capped 24 h, flagged) | |
| Age (§4.2.3) | `apparent_age`, `response_delay`, `corrected_initial_age`, clamped at 0, recomputed per response | |
| Request directives (§5.2.1) | `no-cache`, `max-age=0`, `no-store` (bypass, nothing written), `max-stale[=n]`, `min-fresh=n`, `only-if-cached` -> 504 | |
| Validation (§4.3) | `If-None-Match` (weak compare) / `If-Modified-Since` to the origin; 304 -> `Merge304` (replace, never drop) + `UpdateMeta`; a client's own conditional is answered from a fresh entry | |
| Vary (§4.1) | normalised, recorded as variants under the primary key, `MAX_VARIANTS` LRU per key | `Vary: *` bypasses |
| RFC 5861 | `stale-while-revalidate` (serve STALE, one background refresh per key), `stale-if-error` (stale answers a 5xx/transport error); both off under `must-revalidate` / `proxy-revalidate` | |
| Range (§3.3) | a `Range` request passes through uncached; a stored full entry serves ranges via `http.ServeContent` | 206 responses are never stored; no stitching |
| Hop-by-hop (9110 §7.6.1) | stripped both ways plus `Connection`-listed tokens; `Via: 1.1 edgecache` | `Warning` is never emitted (obsolete) |

Responses carry `X-Cache: HIT|MISS|STALE|REVALIDATED|BYPASS`, `Age`, `X-Cache-Reason` on bypass and
`X-Cache-Key` when `LOG_LEVEL=debug`.

## Storage tiers

- **memstore**: 16 shards (`key[0] % N`), each a map + `container/list` LRU under its own mutex with
  `capacity/N` bytes; bodies are immutable `[]byte` shared with readers, so a hit is a `bytes.Reader`.
- **diskstore** (`EDGECACHE_DISK_DIR`): `<dir>/<aa>/<bb>/<hex>.body` + `.meta`, written to `tmp/`, synced,
  renamed, **meta last** so any partial state is detectable; the index is rebuilt by scanning at start
  (a corrupt meta is deleted and logged, never a startup failure). Entries are refcounted and an
  eviction of a held file goes on a tombstone list the janitor retries (NTFS will not unlink an open file).
- **tiered**: memory in front of disk, promotion on a disk hit that fits `MEM_MAX_OBJECT_BYTES`, fan-out
  writer on a miss. `DISK_DIR=""` gives a pure-memory cache.

## Collapse and orphan fetch

Concurrent misses for one key go through `internal/collapse`: the first caller leads on a context
derived from the engine root (bounded by `COLLAPSE_LEAD_TIMEOUT`), never from its own request, so a
client that disconnects cannot kill the fetch the others wait on; waiters re-`Get` after commit and
count into `edgecache_collapsed_total`. On the tee loop a client write error does not abort the fetch:
the leader keeps filling the store up to `ORPHAN_MAX_BYTES`, then aborts, so a cancelled request still
lands an entry. Proof from the smoke run: 20 concurrent cold `GET /obj/777` -> origin `{"total":1}`,
`edgecache_collapsed_total 19`.

## Purge API

- `PURGE <path>` on the proxy listener with `X-Purge-Token` (constant-time, length-independent compare;
  missing -> 401, wrong -> 403). Deletes every variant of the URL.
- `POST /admin/purge {"url"|"prefix"|"tag"}` on the admin listener; exactly one selector. Prefix and
  tag go through the in-memory tag index (`Cache-Tag` / `Surrogate-Key`, rebuilt from disk metas at boot,
  reconciled with the store's own evictions on every janitor tick so it never outgrows the store).
- With a token configured both listeners require it; without one only loopback peers may purge.
- Response `{"purged":N,"kind":"url|prefix|tag"}` (+ `keys` with `EDGECACHE_PURGE_VERBOSE`), counted in
  `edgecache_purges_total{kind}`.

## Configuration

Every key is validated at startup and all problems are reported at once (`errors.Join`).

| Env | Default | Meaning |
|---|---|---|
| `EDGECACHE_ADDR` / `EDGECACHE_ADMIN_ADDR` | `127.0.0.1:8081` / `127.0.0.1:9101` | proxy listener / admin + metrics + pprof (token required off loopback) |
| `EDGECACHE_ORIGIN` / `EDGECACHE_ORIGIN_HOST` | *(required)* / `""` | upstream base URL (http/https; a client path is dot-segment-resolved before it is appended, so `/../` never leaves the base path; userinfo is redacted in logs); `Host` override |
| `EDGECACHE_ORIGIN_HEADER_TIMEOUT` / `_TIMEOUT` / `_MAX_CONNS` | `10s` / `60s` / `128` | header wait, whole fetch, pool per host |
| `EDGECACHE_MEM_BYTES` / `EDGECACHE_MEM_SHARDS` | `512MiB` / `16` | memory budget, shard count (power of two in [1,256]) |
| `EDGECACHE_DISK_DIR` / `EDGECACHE_DISK_BYTES` | `""` (disabled) / `5GiB` | disk tier root (probed at startup) and budget |
| `EDGECACHE_MAX_OBJECT_BYTES` / `EDGECACHE_MEM_MAX_OBJECT_BYTES` | `64MiB` / `1MiB` | store / memory admission caps |
| `EDGECACHE_MAX_VARIANTS` | `8` | Vary variants per primary key (LRU) |
| `EDGECACHE_HEURISTIC_FRACTION` / `EDGECACHE_HEURISTIC_MAX` | `0.1` / `24h` | §4.2.2 heuristic freshness |
| `EDGECACHE_DEFAULT_SWR` / `EDGECACHE_DEFAULT_SIE` | `0s` / `0s` | used only when the origin sends neither |
| `EDGECACHE_CACHE_AUTHORIZATION` | `false` | override §3.5 for an origin you own |
| `EDGECACHE_ORPHAN_FETCH` / `EDGECACHE_ORPHAN_MAX_BYTES` | `true` / `8MiB` | finish a fetch after the client disconnects |
| `EDGECACHE_PURGE_TOKEN` / `EDGECACHE_PURGE_VERBOSE` | `""` / `false` | purge credential; list purged keys |
| `EDGECACHE_TAG_HEADERS` | `Cache-Tag,Surrogate-Key` | response headers read as surrogate keys |
| `EDGECACHE_JANITOR_INTERVAL` / `EDGECACHE_COLLAPSE_LEAD_TIMEOUT` | `30s` / `60s` | tombstone sweep + tag-index reconcile + gauge tick / leader budget |
| `REQUEST_TIMEOUT` / `SHUTDOWN_TIMEOUT` | `60s` / `15s` | admin mux only / proxy drain budget (teardown stage 2) |
| `SERVICE_NAME` `APP_ENV` `LOG_LEVEL` `LOG_FORMAT` | `edgecache` `dev` `info` `json` | `LOG_LEVEL=debug` adds `X-Cache-Key` |
| `OTEL_ENABLED` / `OTEL_EXPORTER_OTLP_ENDPOINT` | `false` / `http://localhost:4317` | OTLP/gRPC traces + metrics, off by default |

slow-origin (`cmd/slow-origin`): `ORIGIN_ADDR` `ORIGIN_LATENCY` `ORIGIN_JITTER` `ORIGIN_BODY_BYTES`
`ORIGIN_CACHE_CONTROL` (`max-age=60`, `no-store`, or presets `etag` | `vary` | `swr`) `ORIGIN_KEYSPACE`;
`GET /obj/{n}`, `GET|DELETE /admin/hits`.

## Metrics

`GET /metrics` on the admin listener, text exposition, labels are fixed enums only (never a URL or a
client header): `edgecache_requests_total{outcome}`, `edgecache_responses_total{status_class}`,
`edgecache_response_bytes_total{outcome}`, `edgecache_request_duration_seconds`,
`edgecache_ttfb_seconds{outcome}`, `edgecache_origin_requests_total{status_class}`,
`edgecache_origin_duration_seconds`, `edgecache_collapsed_total`, `edgecache_store_bytes{tier}`,
`edgecache_store_entries{tier}`, `edgecache_store_capacity_bytes{tier}`, `edgecache_evictions_total{tier}`,
`edgecache_purges_total{kind}`, `edgecache_variant_evictions_total`, `edgecache_build_info`.
`GET /admin/stats` is the same data as JSON plus store gauges, collapse in-flight, tag-index size and the
non-secret config. Excerpt from the smoke run:

```
edgecache_requests_total{outcome="hit"} 21
edgecache_requests_total{outcome="miss"} 7
edgecache_requests_total{outcome="revalidated"} 1
edgecache_origin_requests_total{status_class="2xx"} 7
edgecache_origin_requests_total{status_class="3xx"} 1
edgecache_collapsed_total 19
edgecache_store_bytes{tier="mem"} 32768
edgecache_store_entries{tier="mem"} 2
edgecache_purges_total{kind="url"} 1
edgecache_purges_total{kind="prefix"} 4
edgecache_build_info{go_version="go1.27.0",version="8edb75d-dirty"} 1
```

## Benchmarks

Machine: AMD Ryzen 7 3700X (8c/16t), Windows 11, Go 1.27.0, oha 1.16. slow-origin at 50 ms +/- 10 ms,
16 KiB bodies, `Cache-Control: max-age=60`, keyspace 1000; edgecache with a 256 MiB memory tier, no disk;
oha `-c 64 -z 30s` per mode. Hit ratio and origin rps come from `/metrics` deltas, never estimated.
Commands: `task load` (`scripts/cache.ps1`), `task pgo` (`scripts/pgo.ps1`), `task bench` (`scripts/bench.ps1`).

### Hit ratio and latency (`scripts/cache.ps1`)

`results/20260919-085016/summary.md`, zipf(s=1) URL list over 1000 keys (`--urls-from-file`, 984 distinct):

| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 57202 | 0.56 | 2.35 | 8.65 | 99.9% | 33 | 1.0000 |
| warm | 48517 | 0.60 | 2.69 | 11.87 | 100.0% | 33 | 1.0000 |
| no-cache | 1049 | 55.96 | 89.53 | 111.85 | 100.0% | 1047 | 1.0000 |
| origin | 1267 | 50.56 | 58.54 | 60.42 | n/a | 1267 | 1.0000 |

Outcome counts: cold 1,715,472 hit / 982 miss / 301 collapsed (zipf: the first request per key is the only
miss); warm 984 revalidated (max-age=60 lapsed once during the run); no-cache 31,434 revalidated, 10 errors,
10,991 collapsed (`no-cache` forces a conditional per request, so the proxy is origin-bound at ~1050 rps, the
same ceiling as the origin control). Hit ratio counts hit + stale + revalidated as served from cache.

### PGO (`scripts/pgo.ps1`, warm mode, A/B/A/B interleaved)

`results/pgo/20260919-085634/summary.md`: 30 s CPU profile under warm load -> `cmd/edgecache/default.pgo`
(216 KB, committed, picked up by `-pgo=auto`), then 3 interleaved rounds of nopgo/pgo, 30 s each:

| mode | rps nopgo | rps pgo | Δ% | p50 nopgo ms | p50 pgo ms | p99 nopgo ms | p99 pgo ms | p99 Δ% |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| warm | 61605 | 63507 | +3.1 | 0.56 | 0.56 | 7.80 | 7.06 | -9.4 |

| round | rps nopgo | rps pgo | p99 nopgo ms | p99 pgo ms |
|---|---:|---:|---:|---:|
| 1 | 53861 | 58086 | 9.91 | 8.18 |
| 2 | 64807 | 65616 | 6.85 | 6.54 |
| 3 | 66148 | 66820 | 6.63 | 6.47 |

Honest reading: the pgo side wins every round on both rps and p99, but the +3 % / -9 % averages sit inside
the round-to-round spread (nopgo 20 %, pgo 14 %), so this is "small and consistent", not a headline. The
hot path is already 0-alloc parsing plus a sharded map lookup, so there is little for PGO to inline; the
profile is kept because it never regressed. A 2-round run earlier the same morning showed +30 % with a
92 % nopgo spread (one round was disturbed by a parallel compile) and is not counted.

### Hot-path microbenchmarks (`scripts/bench.ps1 -Save baseline` -> `.bench/baseline.txt`)

`.bench/baseline.txt`, `-count=6 -benchmem`, `CGO_ENABLED=0`, 16 threads (`benchstat` output):

| benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| KeyBuild (primary + secondary from a request) | 493 ± 2% | 0 | 0 |
| KeyPrimary / KeySecondary | 244 / 243 | 0 | 0 |
| ParseResponse (Cache-Control + dates) | 239 ± 1% | 0 | 0 |
| ParseRequest (request directives) | 118 ± 3% | 0 | 0 |
| EvaluateFresh (freshness decision) | 178 ± 4% | 0 | 0 |
| Age (§4.2.3 arithmetic) | 162 ± 2% | 0 | 0 |
| StripHopByHop (header copy) | 1624 ± 3% | 968 | 16 |
| WriteHeaders (stored headers -> ResponseWriter) | 377 ± 4% | 32 | 2 |
| MemGetHit (sharded LRU lookup, 16 KiB body) | 196 ± 5% | 336 | 2 |
| HitPath (full engine hit, httptest recorder) | 1558 ± 2% | 368 | 4 |

The whole policy layer is allocation-free; a full hit costs ~1.6 us and 4 allocs, and `StripHopByHop` is
the one remaining allocation hot spot (a `http.Header` clone per proxied response), the obvious next
microbenchmark target.

## Teardown

`cmd/edgecache/main.go` stops in six numbered stages with independent budgets, logged `teardown N/6`:
(1) readiness off so `/readyz` answers 503; (2) proxy drain for `SHUTDOWN_TIMEOUT`, on timeout cancel
the engine root (every collapse leader and orphan fetch aborts) and close; (3) admin drain, 5 s;
(4) stop the janitor, wait 5 s for background refreshes and leaders, close idle origin connections;
(5) close the store (disk flushes metas and releases tombstones); (6) OTel flush, 5 s. Under load with a
5 s origin and `SHUTDOWN_TIMEOUT=3s`, stage 2 times out, cancels 16 in-flight leaders and stages 3-6
finish in 20 ms.

## Testing and definition of done

```powershell
task origin                     # slow-origin on 127.0.0.1:8082
task run                        # edgecache on :8081 (admin :9101), EDGECACHE_ORIGIN defaults to the origin above
curl -sD - -o NUL http://127.0.0.1:8081/obj/1   # X-Cache: MISS, then HIT with Age
task check                      # tidy + vet + lint + test:fast
task test                       # go test -race -count=1 ./...
```

Done means `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `go test -race -count=1 ./...`
green, plus for any perf change a fresh `scripts/cache.ps1` table in `results/<ts>/summary.md` and a
`benchstat` comparison against `.bench/baseline.txt`. Every rfc9111 rule is a table test with a fixed
clock; `internal/collapse` is tested with 100 goroutines where the first caller cancels and the other 99
still get the value; `internal/diskstore` tests orphan sweep, corrupt meta and evict-while-reading.

## Follow-ups

- Path-prefix purge that matches on the canonical path rather than the absolute URL prefix.
- Stream-sharing for collapse waiters (Varnish-style) if the cold p99 says the wait for commit dominates.
- Store 206 responses and stitch ranges; today `Range` requests pass through uncached.
- A `HEAD` miss is forwarded uncached (only a stored GET answers HEAD); fetching the GET on a HEAD miss
  would warm the entry for the next GET.
