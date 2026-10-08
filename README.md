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
| Methods (§4) | GET/HEAD cached; HEAD served from a stored GET; a HEAD miss is forwarded and stores nothing; POST/PUT/PATCH/DELETE invalidate the primary key on a non-error response | POST responses are never stored |
| HEAD freshening (§4.3.5) | a HEAD for a stale entry goes upstream; a 200 whose `ETag` / `Last-Modified` (and `Content-Length`, if sent) match the stored GET updates its headers and restarts its freshness like a 304; a mismatch or a HEAD with no validator marks it stale (lifetime 0); a HEAD saying `no-store` / `private` drops it | |
| Storable (§3) | status in {200,203,204,300,301,308,404,405,410,414,451}; `no-store` either side, `private`, `Vary: *` reject; §3.5 `Authorization` rule (`EDGECACHE_CACHE_AUTHORIZATION` overrides) | |
| Freshness (§4.2) | `s-maxage` > `max-age` > `Expires - Date` > heuristic (`0.1 x (Date - Last-Modified)`, capped 24 h, flagged) | |
| Age (§4.2.3) | `apparent_age`, `response_delay`, `corrected_initial_age`, clamped at 0, recomputed per response | |
| Request directives (§5.2.1) | `no-cache`, `max-age=0`, `no-store` (bypass, nothing written), `max-stale[=n]`, `min-fresh=n`, `only-if-cached` -> 504 | |
| Validation (§4.3) | `If-None-Match` (weak compare) / `If-Modified-Since` to the origin; 304 -> `Merge304` (replace, never drop) + `UpdateMeta`; a client's own conditional is answered from a fresh entry | |
| Vary (§4.1) | normalised, recorded as variants under the primary key, `MAX_VARIANTS` LRU per key | `Vary: *` bypasses |
| RFC 5861 | `stale-while-revalidate` (serve STALE, one background refresh per key), `stale-if-error` (stale answers a 5xx/transport error); both off under `must-revalidate` / `proxy-revalidate` | |
| Range (9110 §14) | a stored full 200 answers `Range` itself: single range -> 206 + `Content-Range`, several -> `multipart/byteranges`, none satisfiable -> 416 `bytes */N`; `If-Range` (strong ETag or exact `Last-Modified`, §13.1.5) falls back to 200; malformed, non-`bytes`, > 64 ranges or ranges summing past the body are ignored (200, §14.2). A range **miss** passes through uncached, then one background plain GET (`EDGECACHE_RANGE_FILL`, deduped per key, collapsed with concurrent GET misses) fills the entry when the pass-through answer looks storable and its complete length fits `MAX_OBJECT_BYTES`; a range on a **stale** entry passes through and starts a background revalidation. HEAD ignores `Range`. Parser fuzzed 60 s (`FuzzParseRange`, 8.4 M execs) and the serving path 60 s (`FuzzServeRange`, 2.1 M execs) | 206 responses are never stored (no §3.4 combining); multi-range on a `Content-Encoding` body answers 200 |
| Hop-by-hop (9110 §7.6.1) | stripped both ways plus `Connection`-listed tokens; `Via: 1.1 edgecache` | `Warning` is never emitted (obsolete) |
| Forwarded fields | `X-Forwarded-Proto` picks the key's scheme and `X-Forwarded-For/-Proto/-Host` + `Forwarded` are passed upstream only from `EDGECACHE_TRUSTED_PROXIES` peers (default: nobody); from anyone else they are dropped and rebuilt from the connection, so a plain-HTTP client cannot plant entries under the `https` key | behind a TLS terminator, list it or every entry keys as `http` |

Responses carry `X-Cache: HIT|MISS|STALE|REVALIDATED|BYPASS`, `Age`, `X-Cache-Reason` on bypass and
`X-Cache-Key` when `LOG_LEVEL=debug`.

## Storage tiers

- **memstore**: 16 shards (`key[0] % N`), each a map + `container/list` LRU under its own mutex with
  `capacity/N` bytes; bodies are immutable `[]byte` shared with readers, so a hit is a `bytes.Reader`.
- **diskstore** (`EDGECACHE_DISK_DIR`): `<dir>/<aa>/<bb>/<hex>.body` + `.meta`, written to `tmp/`, synced,
  renamed, **meta last** so any partial state is detectable; the index is rebuilt by scanning at start
  (a corrupt meta is deleted and logged, never a startup failure). Entries are refcounted and an
  eviction of a held file goes on a tombstone list the janitor retries (NTFS will not unlink an open file).
  Measured on this box (5,000 entries of 16 KiB in `%TEMP%`, 2026-10-07): a commit costs ~6.8 ms (two fsyncs),
  and the startup scan takes 23-30 ms warm, but **47 s on the first start after the fill**: Windows Defender
  scans each newly created file on its first open. Put `EDGECACHE_DISK_DIR` on a Defender-excluded path on
  Windows; Linux and containers do not pay it. A persistent index would only move that cost to each entry's
  first read, so the scan stays.
- **tiered**: memory in front of disk, promotion on a disk hit that fits `MEM_MAX_OBJECT_BYTES`, fan-out
  writer on a miss. `DISK_DIR=""` gives a pure-memory cache.

## Collapse and orphan fetch

Concurrent misses for one key go through `internal/collapse`: the first caller leads on a context
derived from the engine root (bounded by `COLLAPSE_LEAD_TIMEOUT`), never from its own request, so a
client that disconnects cannot kill the fetch the others wait on; waiters count into
`edgecache_collapsed_total`.

Waiters do not wait for the commit: once the leader has the origin's headers and the answer is storable, it
publishes a stream (`collapse.DoEarly`) and every waiter serves the body as it downloads, from byte 0, at
the leader's pace. A waiter only takes the stream when the committed entry would have served it (same
Vary-selected key, `Evaluate` says fresh, its own conditional still answers 304), so nothing a waiter could
not have read from the store is shared. The body is retained for late joiners up to
`EDGECACHE_COLLAPSE_STREAM_BYTES`; past that the stream closes to new waiters (they wait for the commit, as
before) and keeps only what an attached waiter has not yet sent. While a waiter is attached the leader reads
the origin body to the end even if its own client left or the store refused the object. `0` turns
streaming off. If the origin body fails after the status line went out, the leader and every waiter abort
the client connection (`http.ErrAbortHandler`) rather than end a chunked body cleanly, so a short body is
never mistaken for a complete one. On the tee loop a client write error does not abort the fetch:
the leader keeps filling the store up to `ORPHAN_MAX_BYTES`, then aborts, so a cancelled request still
lands an entry. Proof from the smoke run: 20 concurrent cold `GET /obj/777` -> origin `{"total":1}`,
`edgecache_collapsed_total 19`.

## Purge API

- `PURGE <path>` on the proxy listener with `X-Purge-Token` (constant-time, length-independent compare;
  missing -> 401, wrong -> 403). Deletes every variant of the URL.
- `POST /admin/purge {"url"|"prefix"|"tag"}` on the admin listener; exactly one selector. A prefix is
  either an absolute URL prefix (`http://cdn.example/static/`, one scheme + host) or path-relative
  (`/static/`): the proxy fronts one origin, so a path prefix matches that path under every host and
  scheme the cache has stored, escape-normalised like the stored URLs (`/%7Euser/` = `/~user/`). Prefix and
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
| `EDGECACHE_TRUSTED_PROXIES` | `""` (nobody) | comma-separated CIDRs / IPs whose `X-Forwarded-*` / `Forwarded` fields are honoured |
| `EDGECACHE_RANGE_FILL` | `true` | a range miss starts one background full GET so the next range is a hit |
| `EDGECACHE_JANITOR_INTERVAL` / `EDGECACHE_COLLAPSE_LEAD_TIMEOUT` | `30s` / `60s` | tombstone sweep + tag-index reconcile + gauge tick / leader budget |
| `EDGECACHE_COLLAPSE_STREAM_BYTES` | `8MiB` | body retained so collapse waiters stream a miss while it downloads; `0` = waiters wait for the commit |
| `REQUEST_TIMEOUT` / `SHUTDOWN_TIMEOUT` | `60s` / `15s` | admin mux only / proxy drain budget (teardown stage 2) |
| `SERVICE_NAME` `APP_ENV` `LOG_LEVEL` `LOG_FORMAT` | `edgecache` `dev` `info` `json` | `LOG_LEVEL=debug` adds `X-Cache-Key` |
| `OTEL_ENABLED` / `OTEL_EXPORTER_OTLP_ENDPOINT` | `false` / `http://localhost:4317` | OTLP/gRPC traces + metrics, off by default |

slow-origin (`cmd/slow-origin`): `ORIGIN_ADDR` `ORIGIN_LATENCY` `ORIGIN_JITTER` `ORIGIN_BODY_BYTES`
`ORIGIN_RATE` (body bytes/s, `0` = unpaced) `ORIGIN_CACHE_CONTROL` (`max-age=60`, `no-store`, or presets `etag` | `vary` | `swr`) `ORIGIN_KEYSPACE`;
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
Commands: `task load` (`scripts/cache.ps1`), `task pgo` (`scripts/pgo.ps1`), `task bench` (`scripts/bench.ps1`),
`pwsh -File scripts/herd.ps1` (collapse streaming A/B).

### Hit ratio and latency (`scripts/cache.ps1`)

`results/20260922-181426/summary.md` (re-run after Q7 changed the handler; **loaded box**: other agents were
building and testing in parallel, so absolute rps is lower than the 2026-09-19 run and is not comparable to it),
zipf(s=1) URL list over 1000 keys (`--urls-from-file`, 984 distinct):

| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 39253 | 0.65 | 3.67 | 14.50 | 99.9% | 33 | 1.0000 |
| warm | 36328 | 0.68 | 4.01 | 16.12 | 100.0% | 33 | 1.0000 |
| no-cache | 1063 | 55.32 | 88.15 | 107.37 | 100.0% | 1063 | 1.0000 |
| origin | 1265 | 50.59 | 58.57 | 60.50 | n/a | 1265 | 1.0000 |

Outcome counts: cold 1,177,383 hit / 984 miss / 291 collapsed (zipf: the first request per key is the only
miss); warm 983 revalidated (max-age=60 lapsed once during the run); no-cache 31,898 revalidated, 10 errors,
11,116 collapsed (`no-cache` forces a conditional per request, so the proxy is origin-bound at ~1060 rps, the
same ceiling as the origin control). Hit ratio counts hit + stale + revalidated as served from cache.

**Q7 A/B** (`results/q7/20260922-180700/summary.md`, loaded box): before = `80898e9`, after = Q7, warm mode 15 s,
6 interleaved ABAB rounds: median rps 30050 -> 29252 (-2.7 %), p50 0.80 -> 0.79 ms, p99 19.9 -> 21.7 ms, against a
round-to-round spread of 37 % / 57 % (rps) and 82 % / 83 % (p99), so no regression outside the noise. The engine
hit benchmark `BenchmarkHandlerHit` (real handler + memstore + metrics, 8 ABAB rounds, benchstat) is 6.95 -> 7.01 us,
p=0.38, **25 allocs/op both sides**; `HitPath` stays 368 B / 4 allocs, p=0.57. Trusted-proxy checks run only when
`X-Forwarded-Proto` is present and `trustproxy.List.Trusts` is 0-alloc (pinned by a test).

### Collapse streaming (`scripts/herd.ps1`, stream off/on, ABBA)

`results/herd/20261007-193209/summary.md`: slow-origin serves a 4 MiB body paced at 8 MiB/s (~500 ms per
download, 20 ms header latency); a fresh edgecache (memory tier only) per run; `cmd/herd` releases 32
concurrent GETs per cold key, 10 keys per run, 3 ABBA pairs. Median of the per-run percentiles:

| stream | role | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |
|---|---|---:|---:|---:|---:|
| off | leader | 25.9 | 39.8 | 525.6 | 537.9 |
| off | waiter | 540.9 | 577.8 | 561.4 | 596.5 |
| on | leader | 27.4 | 48.5 | 522.6 | 533.1 |
| on | waiter | 26.4 | 52.5 | 522.7 | 534.1 |

With streaming off a waiter's first byte arrives after the whole download plus the commit (541 ms); with it
on, at the same time as the leader's (26.4 ms, -95 %), and its total drops to the leader's (p99 597 -> 534 ms)
because the copy out of the store no longer follows the download. All 960 requests per mode returned the
full 4 MiB body; every run's rows are in the summary. The leader is within noise (the first run,
`results/herd/20261007-191712/`, taken before the last stream change, agrees: waiter TTFB 539 -> 25.6 ms).

Cost on a miss nobody joins (`BenchmarkHandlerMiss`, 16 KiB, memstore, 6 runs, benchstat): +16.7 KiB/op (the
body copy kept for late joiners) and +5 allocs/op (78 -> 83); ns/op inside the noise (p=0.39).

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

`HitPath` replays the hit steps by hand (key, Get, Evaluate, header copy, body copy); it does not go through
`cache.Handler`. The handler-level number is `BenchmarkHandlerHit` in `internal/cache` (Q7): ~7 us and 25 allocs
per 16 KiB hit on the loaded box (`results/q7/20260922-180700/summary.md`), the difference being the request
struct, `absoluteURL`, variant lookup, the recorder and the slog call. None of the baseline rows above measure
code Q7 changed, so `.bench/baseline.txt` stands.

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
- Store 206 responses and combine them (RFC 9111 §3.4); today a range miss passes through and a
  background full GET fills the entry instead, which fetches the object twice on the first miss.
- A `HEAD` miss is forwarded uncached (only a stored GET answers HEAD); fetching the GET on a HEAD miss
  would warm the entry for the next GET.
