# ADR-0001: edgecache - RFC 9111 HTTP caching reverse proxy
Status: Proposed | Date: 2026-09-19 | Module: `github.com/crowbett/edgecache` (Go 1.27)

## Context
P10 in `~/dev/ROADMAP.md` Queue 2: RFC 9111 caching reverse proxy - freshness/validation, swr, Vary, singleflight
collapse, memory + disk tiers, purge API, Prometheus; the numbers are hit-ratio and p99 with oha against a slow
origin, before/after PGO. The repo does not exist yet: the integrator runs `new-project.ps1 -Template go-api -Name
edgecache` and moves this file to `docs/PLAN.md`. The scaffold gives `cmd/api/main.go` (config -> logger -> otel ->
`http.Server` -> `signal.NotifyContext`), `internal/config` (env only), `internal/http` (`NewRouter` ->
`registerRoutes` + `wrap` = RequestID/Tracing/Logging/Timeout/Recover/TagRoute), `internal/otel`; Postgres/compose/
redis get deleted. Style comes from two finished siblings. `~/dev/go/aigw`: `internal/proxy` streams the upstream body
on the handler goroutine with no buffering; `internal/gateway` puts streaming routes on a second mux **outside**
`http.TimeoutHandler` (its writer cannot flush, its deadline guillotines long responses); `internal/metrics` is
hand-written Prometheus text with a golden test, no `client_golang`; `internal/config` validates at startup with
joined errors; `cmd/aigw/main.go` tears down in numbered stages with independent budgets. `~/dev/go/perf-lab`:
`scripts/_lib.ps1` as the shared contract plus `load.ps1`/`pgo.ps1`/`bench.ps1`, results under
`results/<experiment>/<yyyyMMdd-HHmmss>/` with only `summary.md` committed, `.bench/baseline.txt` + benchstat,
`default.pgo` committed beside the binary's package. Non-goals (v1): TLS termination, multi-origin routing (aigw's
job), clustered cache, caching 206/ranges, caching POST bodies, `Warning` headers (obsolete, §5.5).

## Decision
Three layers with hard boundaries. `internal/rfc9111` is **pure policy** - header bytes in, decisions out, no I/O and
no clock of its own (`now time.Time` is always a parameter), which turns the spec into a table test. `internal/store`
+ `memstore`/`diskstore`/`tiered` are **dumb keyed blob storage**: bytes, budgets, refcounts, atomic publish, zero
HTTP semantics. `internal/cache` is the **engine** joining them (key -> policy -> origin -> stream); `internal/server`
takes aigw's two-mux idea further - the catch-all proxy gets its own listener, admin/metrics/pprof a second, loopback
one. A miss is streamed: bytes go to the store writer and the client in one loop, and a client write error does **not**
abort the origin fetch (up to `ORPHAN_MAX_BYTES`), so a cancelled request still lands an entry, which is the whole
point in front of a slow origin. Concurrent misses collapse through a hand-written `internal/collapse` whose leader
runs on a **detached** context, so the first client disconnecting cannot kill the fetch 50 others wait on. Deviation
from the template, called out: the proxy handler is not wrapped in `Timeout`/`http.TimeoutHandler` (aigw's SSE
lesson); its budget comes from the origin context. **Zero new dependencies** - `net/http`, `crypto/sha256`,
`container/list`, `encoding/json`, `log/slog` cover all of it; the template's OTel stack stays intact, off by default.

## Package tree
```
cmd/edgecache/main.go   wiring only: config -> logger -> otel -> store -> origin -> cache -> 2 servers -> staged teardown
cmd/slow-origin/main.go benchmark origin: configurable latency/jitter/body size/cache headers/keyspace
internal/config/        env -> Config; joined validation at startup; no globals
internal/http/          template middleware chain, unchanged
internal/cachekey/      Key, canonical URL, Vary normalisation, primary/secondary derivation
internal/rfc9111/       pure policy: directives, freshness, validation, Age, 304 merge. stdlib only
internal/store/         port types: Meta, Entry, Variant, Writer, Stats + sentinels. No I/O
internal/memstore/      sharded LRU with a byte budget; bodies are []byte
internal/diskstore/     content-addressed files + index; temp+rename; scan recovery; refcounts; tombstones
internal/tiered/        memory in front of disk; promotion on disk hit; fan-out writer
internal/collapse/      generic per-key singleflight, detached leader ctx, waiter counts, TryStart
internal/origin/        upstream client: base URL, Host override, hop-by-hop strip, retry-once
internal/tagindex/      surrogate keys (Cache-Tag/Surrogate-Key) + URL prefixes -> keys, for purge
internal/cache/         the engine: Handler, hit/miss/revalidate/stale paths, tee copy, Purge
internal/metrics/       hand-written Prometheus text registry + http.Handler (aigw/sysmon style)
internal/server/        route tables, 2-listener wiring, admin handlers, PURGE auth
internal/bench/         Go microbenchmarks for the hot path
```
Imports flow one way: `server -> cache -> {rfc9111, cachekey, store, origin, collapse, tagindex, metrics}`; `tiered ->
{store, memstore, diskstore}`; `memstore|diskstore -> store`; `store -> rfc9111` (embedded `Resp` value type);
`rfc9111`, `cachekey` -> stdlib only. No cycles, no `util`/`common`/`models`.

## Interfaces and key types
```go
// internal/cachekey - Key is an array, never a slice: no alloc, no escape.
type Key [32]byte
func Canonical(u *url.URL) string                       // lowercase host, default port dropped, unreserved %-decoded, query sorted
func Primary(method string, u *url.URL) Key             // sha256(method \x00 canonical)
func Secondary(p Key, vary []string, h http.Header) Key // sha256(p ‖ name \x00 value ...), pooled scratch buffer
func NormalizeVary(v []string) ([]string, error)        // lowercase+sort+dedupe; ErrVaryStar on "*"
func (k Key) String() string                            // hex; logs and admin only

// internal/rfc9111 - pure policy, the clock is always a parameter. Both directive structs also carry Has*
// flags (HasMaxAge, HasMaxStale, MaxStaleAny, HasMinFresh / HasMaxAge, HasSMaxAge, HasSWR, HasSIE).
type ReqDirectives struct{ NoStore, NoCache, OnlyIfCached, NoTransform bool; MaxAge, MaxStale, MinFresh time.Duration }
type RespDirectives struct {
	NoStore, NoCache, Private, Public, MustRevalidate, ProxyRevalidate, Immutable bool
	NoCacheFields, PrivateFields                                                  []string // qualified forms
	MaxAge, SMaxAge, StaleWhileRevalidate, StaleIfError                           time.Duration
}
// Resp is what the store persists about one origin exchange; Header has hop-by-hop stripped.
type Resp struct {
	Status                    int
	Header                    http.Header
	RequestTime, ResponseTime time.Time // §4.2.3 request_time / response_time
	Lifetime                  time.Duration
	Heuristic                 bool
}
// Policy is injected from config, never global: 0.1 of (Date - Last-Modified) capped at 24h, plus an escape
// hatch for the §3.5 Authorization rule on an origin you own. Reason is one of ~20 constants.
type Policy struct{ HeuristicFraction float64; HeuristicMax time.Duration; CacheAuthorization bool }
type Action int // Fresh | Stale | Revalidate | StaleWhileRevalidate | Bypass | GatewayTimeout
type Decision struct{ Action Action; Age time.Duration; Background bool; Reason Reason }
func ParseRequest(h http.Header) ReqDirectives   // quoted-string aware
func ParseResponse(h http.Header) RespDirectives
func (p Policy) Storable(r *http.Request, rd ReqDirectives, res Resp, sd RespDirectives) (bool, Reason)
func (p Policy) Lifetime(res Resp, sd RespDirectives) (time.Duration, bool) // s-maxage>max-age>Expires>heuristic
func (p Policy) Evaluate(now time.Time, res Resp, sd RespDirectives, rd ReqDirectives) Decision
func Age(res Resp, now time.Time) time.Duration      // §4.2.3, clamped at 0
func ConditionalRequest(h http.Header, res Resp)     // If-None-Match / If-Modified-Since
func Merge304(stored, fresh http.Header) http.Header // §4.3.4: replace, never drop
func StripHopByHop(h http.Header) http.Header        // + Connection-listed tokens

// internal/store - types only; the interfaces live in the consumers.
type Meta struct {
	rfc9111.Resp
	Method, URL, ETag string
	Vary, Tags        []string // Vary normalised; empty = the no-vary variant
	Size              int64
	LastModified      time.Time
}
type Variant struct{ Vary []string; Key cachekey.Key; Seen time.Time }
type Entry struct{ Key cachekey.Key; Meta Meta; Tier string } // Tier: "mem" | "disk"
func (e *Entry) Open() (io.ReadCloser, error)                 // mem: *bytes.Reader; disk: *os.File
func (e *Entry) Release()                                     // drops the refcount Get took; mandatory
type Writer interface {
	io.Writer
	Commit(final Meta) (*Entry, error) // atomic publish, returns a held entry
	Abort() error                      // idempotent; safe in a defer after Commit
}
type Stats struct{ Bytes, Entries, Evictions, Capacity int64 }
var ErrMiss, ErrTooLarge, ErrClosed = errors.New("store: miss"), errors.New("store: too large"), errors.New("store: closed")

// internal/cache - interfaces defined where they are consumed; memstore/diskstore/tiered satisfy Store.
type Store interface {
	Get(ctx context.Context, k cachekey.Key) (*store.Entry, error)
	Writer(ctx context.Context, k cachekey.Key, m store.Meta) (store.Writer, error)
	UpdateMeta(ctx context.Context, k cachekey.Key, m store.Meta) error // 304 merge; body untouched
	Delete(ctx context.Context, keys ...cachekey.Key) (int, error)
	Variants(ctx context.Context, primary cachekey.Key) ([]store.Variant, error)
	PutVariant(ctx context.Context, primary cachekey.Key, v store.Variant) error
	Stats() map[string]store.Stats // keyed by tier
	Close() error
}
type Fetcher interface{ Do(ctx context.Context, r *http.Request) (*http.Response, error) } // *origin.Origin
type Recorder interface { // *metrics.Registry satisfies it
	Request(outcome string, status int, ttfb, total time.Duration, bytes int64)
	Origin(status int, d time.Duration, err error)
	Collapsed(waiters int)
	StoreGauges(map[string]store.Stats)
}
type PurgeRequest struct{ URL, Prefix, Tag string }
type PurgeResult struct{ Purged int; Keys []string } // Keys only when PURGE_VERBOSE
func New(cfg Config, s Store, f Fetcher, rec Recorder, log *slog.Logger) (*Cache, error)
func (c *Cache) Handler() http.Handler           // the catch-all proxy handler
func (c *Cache) Purge(ctx context.Context, req PurgeRequest) (PurgeResult, error)
func (c *Cache) Run(ctx context.Context)         // janitor + gauge refresh; returns on ctx done
func (c *Cache) Drain(ctx context.Context) error // waits for in-flight background refreshes

// internal/collapse - ~90 generic lines, replaces x/sync/singleflight (see Alternatives). Do's leader runs fn
// on a context derived from root, NOT from the caller: a waiter cancelling never kills the shared fetch.
type Group[T any] struct{ /* mu + map[string]*call[T] */ }
func New[T any](root context.Context, lead time.Duration) *Group[T]
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, int, error) // val, shared, waiters
func (g *Group[T]) TryStart(key string) (done func(), ok bool) // background revalidation dedupe
func (g *Group[T]) InFlight() int

// internal/origin
type Config struct {
	Base                              *url.URL
	HostOverride, Via                 string
	DialTimeout, HeaderTimeout, Total time.Duration
	MaxConnsPerHost, MaxIdleConns     int
}
func New(cfg Config, log *slog.Logger) (*Origin, error)                           // concrete return
func (o *Origin) Do(ctx context.Context, r *http.Request) (*http.Response, error) // retry once: idempotent + conn error
func (o *Origin) CloseIdleConnections()

// internal/tagindex (in-memory, rebuilt from store metas during disk recovery) and internal/metrics
func NewIndex() *Index
func (ix *Index) Add(k cachekey.Key, url string, tags []string)
func (ix *Index) Remove(k cachekey.Key)
func (ix *Index) ByTag(tag string) []cachekey.Key
func (ix *Index) ByPrefix(prefix string) []cachekey.Key
func NewRegistry(build map[string]string) *Registry
func (m *Registry) Handler() http.Handler // text/plain; version=0.0.4
func (m *Registry) SnapshotJSON() []byte  // /admin/stats
```
Families: `edgecache_requests_total{outcome,status}` (`outcome=hit|miss|stale|revalidated|bypass|error`),
`edgecache_request_duration_seconds`, `edgecache_ttfb_seconds{outcome}`, `edgecache_origin_requests_total{status}`,
`edgecache_origin_duration_seconds`, `edgecache_collapsed_total`, `edgecache_store_bytes{tier}`,
`edgecache_store_entries{tier}`, `edgecache_evictions_total{tier}`, `edgecache_variant_evictions_total`,
`edgecache_purges_total{kind}`, `edgecache_build_info`. Labels are fixed enums only - never a URL, never a client
header. Responses carry `X-Cache: HIT|MISS|STALE|REVALIDATED|BYPASS`, `Age`, `X-Cache-Reason` on bypass,
`X-Cache-Key` in debug builds.

## Cache semantics (RFC 9111 / 9110 / 5861)
- **Methods**: `GET`/`HEAD` cacheable, `HEAD` served from a stored `GET` (headers only). POST is not cached even with
  explicit freshness (§4 allows it; body-in-key plus invalidation is not worth it) but, like `PUT`/`PATCH`/`DELETE`,
  **invalidates** the primary key on a non-error response (§4.4). Everything else bypasses.
- **Storable** (§3): status in {200,203,204,300,301,308,404,405,410,414,451}; no `no-store` on either side; no
  `Vary: *`; not `private`; and either explicit freshness or a heuristically cacheable status. With `Authorization`,
  only when `public`/`s-maxage`/`must-revalidate` is present (§3.5).
- **Freshness** (§4.2): `s-maxage` > `max-age` > `Expires - Date` > heuristic. Heuristic applies only to a
  heuristically cacheable status with `Last-Modified` and no freshness directive: `min(0.1*(Date-LastModified), 24h)`,
  flagged `Heuristic`. `Age` per §4.2.3 including `response_delay`, clamped at 0, recomputed per response.
- **Request directives** (§5.2.1): `no-cache` and `max-age=0` force revalidation; `no-store` bypasses and writes
  nothing; `max-stale[=n]` widens and `min-fresh=n` narrows acceptance; `only-if-cached` returns **504** whenever the
  answer would need the origin.
- **Validation** (§4.3): conditional `If-None-Match` (stored ETags, weak compare) and/or `If-Modified-Since`. `304` ->
  `Merge304` (field-by-field replace, never drop) + refreshed times + recomputed lifetime -> `UpdateMeta` ->
  `REVALIDATED`; non-304 replaces the entry. A *client's* own conditional is answered from a fresh stored entry.
- **Vary** (§4.1): the response's `Vary` is normalised and recorded as a `Variant` under the primary key, capped at
  `MAX_VARIANTS` (LRU; an evicted variant takes its entry with it). Lookup walks the variant list, computes each
  secondary key, first match wins. `Vary: *` -> bypass.
- **swr / sie** (RFC 5861): inside `stale-while-revalidate` the stale entry is served at once as `STALE` while one
  background revalidation per key runs (`TryStart`); inside `stale-if-error` a stale entry answers a 5xx or transport
  error. Both disabled by `must-revalidate`/`proxy-revalidate`.
- **Range** (§3.3): a `Range` request passes through uncached and is never stored; a stored *full* entry serves ranges
  via `http.ServeContent`. Storing 206s and stitching is deferred.
- **Caps**: `MAX_OBJECT_BYTES` (64 MiB) aborts the writer and degrades to passthrough mid-stream; memory admits
  objects <= `MEM_MAX_OBJECT_BYTES` (1 MiB); disk cap 5 GiB. `Warning` is never emitted.

## Storage
`sha256` at both key levels: Zen 2 has SHA-NI, the digest is a fixed `[32]byte` value, and the same digest
content-addresses disk files. **memstore**: 16 shards picked by `key[0] % N`, each a `map[Key]*node` +
`container/list` LRU under its own mutex with `capacity/N` bytes; eviction pops tails until the incoming object fits,
counting `edgecache_evictions_total{tier="mem"}`; bodies are immutable `[]byte` shared with readers, so `Open()` is a
`bytes.Reader` and eviction is a map delete. **diskstore**: `<dir>/<aa>/<bb>/<hex>.body` + `.meta` (JSON), written to
`<dir>/tmp/<hex>.<rand>`, `Sync`ed, then `os.Rename`d (atomic on NTFS via `MoveFileEx`), **meta last** - a `.body`
with no `.meta` is a partial write and gets swept. The index (`map[Key]*fileEntry`, LRU order, byte total) is rebuilt
at start by walking the tree and reading metas; a corrupt meta deletes the pair and logs, never failing startup.
Entries are **refcounted** (`Get` holds, `Release` drops) and eviction of a held entry goes on a tombstone list the
janitor retries - NTFS will not unlink an open file. **tiered**: `Get` tries memory then disk, promoting a disk hit
that fits; `Writer` fans out into a memory buffer *and* the disk writer, dropping the memory half past the memory cap.
`DISK_DIR=""` gives a pure-memory cache.

## Data flow
1. Proxy mux (no `TimeoutHandler`) -> RequestID/Tracing/Logging/Recover -> `cache.Handler`.
2. Method check + `ParseRequest`. Unsafe method -> invalidate primary key, pass through, `bypass`.
3. `k0 := Primary(method, r.URL)` -> `Variants(k0)` -> per variant `Secondary` -> `Store.Get`.
4. **Hit**, by `Evaluate`: `Fresh` -> stored headers + `Age` + `X-Cache: HIT`, `io.Copy` from `Entry.Open()`,
   `Release`. `StaleWhileRevalidate` -> same but `STALE`, then `TryStart(key)` spawns the background refresh on the
   root context. `Revalidate` -> conditional origin call; `304` -> merge + `UpdateMeta` -> `REVALIDATED`; `200` ->
   store + serve `MISS`; 5xx/err inside `stale-if-error` -> `STALE`. `only-if-cached` with no usable entry -> `504`.
5. **Miss**: `collapse.Do(key)` - one leader, `waiters` into `edgecache_collapsed_total`. The leader calls `origin.Do`
   on the detached context, checks `Storable`, opens `Store.Writer` and runs the tee loop: read chunk -> write to
   store (always) -> write to client (until it errors; after that it keeps filling the store up to
   `ORPHAN_MAX_BYTES`, then `Abort`s). `Commit` publishes atomically and registers the `Variant` + `tagindex` entries.
   Waiters wake after commit and re-`Get` (v1 waits, see Alternatives); a waiter whose context died drops out. Not
   storable -> no writer, plain copy, `bypass`.
6. **Purge**: `PURGE <path>` on the proxy listener (`X-Purge-Token`, constant-time compare, off unless the token is
   configured) or `POST /admin/purge {"url"|"prefix"|"tag"}` on the admin listener. URL -> primary key -> delete every
   variant; prefix/tag -> `tagindex` -> batch delete. `{"purged":N}` + `edgecache_purges_total{kind}`.
7. Every path ends in one `Recorder.Request(...)` and one slog line (`outcome, status, age_s, ttfb_ms, dur_ms, bytes,
   collapsed, key, request_id`).

## Config
| Env | Default | Meaning |
|---|---|---|
| `EDGECACHE_ADDR` / `_ADMIN_ADDR` | `127.0.0.1:8081` / `127.0.0.1:9101` | proxy listener / admin+metrics+pprof |
| `EDGECACHE_ORIGIN` / `_ORIGIN_HOST` | *(required)* / `""` | upstream base URL (http/https); `Host` override |
| `EDGECACHE_ORIGIN_HEADER_TIMEOUT` / `_TIMEOUT` / `_MAX_CONNS` | `10s` / `60s` / `128` | header wait, whole fetch, pool |
| `EDGECACHE_MEM_BYTES` / `_MEM_SHARDS` | `512MiB` / `16` | memory budget, shard count (power of two) |
| `EDGECACHE_DISK_DIR` / `_DISK_BYTES` | `""` (disabled) / `5GiB` | e.g. `%LOCALAPPDATA%\edgecache` |
| `EDGECACHE_MAX_OBJECT_BYTES` / `_MEM_MAX_OBJECT_BYTES` | `64MiB` / `1MiB` | store / memory admission caps |
| `EDGECACHE_MAX_VARIANTS` | `8` | Vary variants per primary key (LRU) |
| `EDGECACHE_HEURISTIC_FRACTION` / `_HEURISTIC_MAX` | `0.1` / `24h` | §4.2.2 heuristic freshness |
| `EDGECACHE_DEFAULT_SWR` / `_DEFAULT_SIE` | `0s` / `0s` | used only when the origin sends neither |
| `EDGECACHE_CACHE_AUTHORIZATION` | `false` | override §3.5 for an origin you own |
| `EDGECACHE_ORPHAN_FETCH` / `_ORPHAN_MAX_BYTES` | `true` / `8MiB` | finish a fetch after client disconnect |
| `EDGECACHE_PURGE_TOKEN` / `_PURGE_VERBOSE` | `""` (PURGE off) / `false` | purge auth, key listing |
| `EDGECACHE_TAG_HEADERS` | `Cache-Tag,Surrogate-Key` | response headers read as surrogate keys |
| `EDGECACHE_JANITOR_INTERVAL` / `_COLLAPSE_LEAD_TIMEOUT` | `30s` / `60s` | tombstone+gauge tick / leader budget |
| `REQUEST_TIMEOUT` / `SHUTDOWN_TIMEOUT` | `60s` / `15s` | admin mux only / stage-2 drain |
| `LOG_LEVEL` `LOG_FORMAT` `OTEL_ENABLED` `OTEL_EXPORTER_OTLP_ENDPOINT` | template | unchanged |
| slow-origin: `ORIGIN_ADDR` `ORIGIN_LATENCY` `ORIGIN_JITTER` `ORIGIN_BODY_BYTES` `ORIGIN_CACHE_CONTROL` `ORIGIN_KEYSPACE` | `:8082` `50ms` `10ms` `16384` `max-age=60` `1000` | benchmark origin |

`config.Load` returns `errors.Join` of every problem at once (aigw style): URL/byte/duration parses,
`MEM_MAX_OBJECT_BYTES <= MEM_BYTES`, shards a power of two in [1,256], disk dir writable (create + remove a temp
file), `HEURISTIC_FRACTION` in (0,1]. No globals; `Config` is passed by value.

## Concurrency and teardown
| Goroutine | Started by | Stopped by | On failure |
|---|---|---|---|
| proxy accept loop | `main` (`proxySrv.ListenAndServe`) | stage 2 graceful stop | err -> `errCh` -> `run` returns |
| admin accept loop | `main` | stage 3 graceful stop (5 s) | err -> `errCh` |
| per-request tee copy | handler goroutine, none extra | body EOF / `ORPHAN_MAX_BYTES` | client write err -> keep filling store, then `Abort` |
| collapse leader | first misser, inline on the **detached** ctx | commit/abort or `COLLAPSE_LEAD_TIMEOUT` | error delivered to every waiter, no entry written |
| background revalidate | `cache` via `collapse.TryStart` | root ctx cancel; `Cache.Drain` waits (WaitGroup) | logs, keeps the stale entry, `outcome="error"` |
| janitor (tombstones + gauges) | `cache.Run` ticker | root ctx cancel | retries next tick |

Teardown in `cmd/edgecache/main.go`, orders/aigw-style numbered stages with independent budgets: (1) `ready.Set(false)`
so `/readyz` answers 503 at once; (2) `proxySrv.Shutdown(SHUTDOWN_TIMEOUT)`, and on `DeadlineExceeded` cancel the cache
root context then `Close()`; (3) `adminSrv.Shutdown(5s)`; (4) cancel the cache root ctx, `cache.Drain(5s)`,
`origin.CloseIdleConnections()`; (5) `store.Close()` - memstore drops, diskstore flushes metas and releases tombstones;
(6) deferred OTel flush (5 s, template default).

## Benchmarks (perf-lab harness shape)
- `scripts/_lib.ps1` copied from perf-lab (repo root, timestamped out dir, oha JSON -> rows, `summary.md` writer). Raw
  `*.json`/`*.log`/`*.pprof` gitignored; only `summary.md` committed.
- `scripts/cache.ps1`: starts `bin/slow-origin.exe` (50 ms ±10 ms, 16 KiB bodies, keyspace 1000, `max-age=60`) and
  `bin/edgecache.exe` in front, warms, then oha `-c 64 -z 30s`. Distribution: preferred is a Zipf-weighted URL list
  (`oha --urls-from-file`) - **check `oha --help` on the installed build first** - else `--rand-regex-url
  'http://127\.0\.0\.1:8081/obj/[0-9]{1,3}'` (uniform), recording which mode ran. Rows: cold, warm, warm + `no-cache`
  clients, direct-to-origin control; columns `mode | rps | p50 | p90 | p99 | hit-ratio | origin-rps`, hit ratio from
  `/metrics` deltas, never estimated.
- `scripts/pgo.ps1`: perf-lab's, pointed at edgecache; profile collected under warm load into
  `cmd/edgecache/default.pgo` (committed), then A/B/A/B interleaved rounds. README headline: warm hit-ratio + p99
  before/after PGO, `results/<ts>/summary.md` committed.
- `scripts/bench.ps1` + `internal/bench`: `BenchmarkKeyBuild` (primary + secondary over 2 vary headers),
  `BenchmarkMemGetHit`, `BenchmarkEvaluateFresh`, `BenchmarkWriteHeaders`, `BenchmarkHitPath` (through an
  `httptest.ResponseRecorder`). Targets: hit path < 2 us and <= 4 allocs, key build 0 allocs. `-count=10 -benchmem`
  saved as `.bench/baseline.txt` in its own commit, benchstat on every perf change.

## Implementation plan
1. **Integrator prep**: scaffold, delete DB/compose/redis, rewrite `internal/config` per the table + `config_test.go`,
   land stubs carrying every signature above so all lanes compile. `go build ./... ; go test ./internal/config/`.
2. **cachekey**: `Canonical`, `Primary`, `Secondary`, `NormalizeVary`; tables for case, default port, `%7E`, query
   order, empty query, `Vary: *`. `go test ./internal/cachekey/ -race` and `-bench Key -benchmem` (0 allocs).
3. **rfc9111 directives + storability**: parsers (quoted strings, qualified `no-cache="Set-Cookie"`), `Storable`,
   `StripHopByHop`, tables from the RFC's examples. `go test ./internal/rfc9111/ -run 'Parse|Storable' -race`.
4. **rfc9111 freshness + validation**: `Lifetime`, `Age`, `Evaluate` (every action incl. `only-if-cached`,
   `max-stale`, `min-fresh`, swr/sie), `ConditionalRequest`, `Merge304`; fixed clock everywhere.
   `go test ./internal/rfc9111/ -race -count=1`.
5. **store types + memstore**: sharded LRU, budget, admission, eviction counters, refcounts. Concurrency test: 8
   writers + 32 readers, bytes never exceed capacity, no held entry freed. `go test ./internal/memstore/ -race -count=2`.
6. **diskstore**: layout, temp+rename+`Sync`, meta JSON, scan recovery, tombstones, janitor hook. Tests: hand-written
   orphan `.body` swept, corrupt meta, evict-while-reading on Windows, budget. `go test ./internal/diskstore/ -race` (`t.TempDir()`).
7. **tiered**: promotion, fan-out writer, per-tier stats, disk-disabled mode. `go test ./internal/tiered/ -race`.
8. **collapse**: generic group, detached leader, waiter counts, `TryStart`. Test: 100 goroutines and 1 fetch, the
   first caller cancels immediately and the other 99 still get the value; leader timeout.
   `go test ./internal/collapse/ -race -count=5`.
9. **origin + slow-origin**: transport tuning, Host override, hop-by-hop strip, `Via`, retry-once on idempotent
   connection errors (never with a consumed body); `cmd/slow-origin` with its flags/env.
   `go test ./internal/origin/ -race` ; `go run ./cmd/slow-origin` + `curl -v 127.0.0.1:8082/obj/1`.
10. **cache engine - miss & hit**: handler, key derivation, variant registration, tee copy, orphan fetch, passthrough,
    `X-Cache`/`Age`; a client disconnecting mid-body must still commit the entry and the next request must be a HIT.
    `go test ./internal/cache/ -run 'Miss|Hit|Orphan|Vary' -race`.
11. **cache engine - revalidate/stale/invalidate + tagindex + purge**: conditionals, `Merge304`, the swr background
    path, stale-if-error, unsafe-method invalidation, tag extraction, `Purge`.
    `go test ./internal/cache/ ./internal/tagindex/ -race -count=1`.
12. **metrics + server + wiring + numbers**: registry with a golden exposition (`testdata/metrics.golden`),
    two-listener server, PURGE auth, `/admin/stats`, staged teardown in `cmd/edgecache`, then the scripts,
    `.bench/baseline.txt`, README numbers and `results/<ts>/summary.md`. `go build ./... ; go vet ./... ; go test
    -race -count=1 ./... ; golangci-lint run ./...` then `scripts/cache.ps1` and `scripts/pgo.ps1`.

## Fanout: 4 workstreams + integrator, exclusive file ownership
- **Integrator** owns `cmd/**`, `internal/config/**`, `internal/http/**`, `internal/otel/**`, `Taskfile.yml`,
  `README.md`, `CLAUDE.md`, `go.mod`/`go.sum`, `scripts/**`: step 1 first (it unblocks everyone), then step 12's
  wiring and the benchmark runs. Only the integrator runs `go get`/`go mod tidy`.
- **WS-A "semantics"**: `internal/rfc9111/**`, `internal/cachekey/**` - steps 2-4; publishes
  `internal/rfc9111/testdata/*.json` fixtures the other lanes read.
- **WS-B "storage"**: `internal/store/**`, `internal/memstore/**`, `internal/diskstore/**`, `internal/tiered/**` - steps 5-7.
- **WS-C "engine"**: `internal/cache/**`, `internal/collapse/**`, `internal/origin/**`, `cmd/slow-origin/**` (handed to
  the integrator at step 12) - steps 8-11.
- **WS-D "observability"**: `internal/metrics/**`, `internal/tagindex/**`, `internal/server/**`, `internal/bench/**`,
  `docs/**` - the metrics/server/bench halves of step 12.
Frozen at step 1, changed only by the integrator: `store.Meta`, `store.Writer`, `cache.Store`, `cache.Fetcher`,
`cache.Recorder`, `rfc9111.Resp`, `rfc9111.Decision`, `config.Config`. A lane needing a change files it rather than
editing in place.

## Alternatives considered
- **`httputil.ReverseProxy` as the base**: free hop-by-hop handling and streaming, but the tee, the collapse leader,
  conditional revalidation and the 304 merge smear across `Director`/`ModifyResponse`/`ErrorHandler`, and
  `ModifyResponse` cannot own the body copy - exactly where the cache lives. aigw proved the explicit copy loop is
  ~120 readable lines; hop-by-hop is 30 in `StripHopByHop`.
- **`golang.org/x/sync/singleflight`**: correct, but `Do` takes no context (the leader inherits whatever it closed
  over, so a leader whose client vanished takes the waiters with it), it cannot report waiter counts for
  `edgecache_collapsed_total`, and `Forget` does not express "one background revalidation per key". ~90 generic lines
  beat the import; listed so the reviewer knows it was weighed, not missed.
- **Stream-sharing waiters** (Varnish-style): better cold p99 on large objects, but needs a shared ring with
  per-reader offsets, backpressure from the slowest client, and a story for a waiter outliving the leader. v1 waits
  for commit; revisit if the cold p99 says the wait dominates.
- **`prometheus/client_golang`**: a dependency tree for twelve families on a loopback daemon; sysmon and aigw already
  ship the hand-written exposition plus its golden test. Rejected on stdlib-first.
- **bbolt/pebble for the disk index**: crash-safer than scan recovery, but it is a dependency and P11 (kvlite) is
  literally the project that earns the right to one. 5 GiB of 16 KiB objects is ~320k metas - measurable at startup.
- **One listener, one mux** (aigw's shape): with a catch-all `/` the admin routes sit on the public port and shadow
  any origin path under `/admin`. Two listeners; the admin one is loopback by default.

## Risks
- **Vary explosion**: an origin varying on `User-Agent` or `Cookie` turns one URL into thousands of entries.
  `MAX_VARIANTS=8` LRU per primary key, `edgecache_variant_evictions_total`, warn-once per key hitting the cap. An
  allowlist would be incorrect, so there isn't one.
- **Disk index corruption**: meta-after-body plus temp+rename makes every partial state detectable, and recovery
  deletes rather than guesses. A corrupt meta is a `WARN` with the path, never a startup failure.
- **Windows delete-while-open**: NTFS refuses to unlink a file with an open handle, so evicting an entry a slow client
  is streaming fails. Refcounts + a janitor tombstone list; the budget counts tombstones as resident so the cap is
  never silently overshot.
- **Large bodies**: `MAX_OBJECT_BYTES` aborts the writer and degrades to passthrough mid-stream (invisible to the
  client), `MEM_MAX_OBJECT_BYTES` stops one file flushing the memory tier.
- **Clock skew for `Age`**: an origin `Date` ahead of ours makes `apparent_age` negative and an entry look fresher
  than it is. Clamp at 0 per §4.2.3, `corrected_initial_age = max(apparent_age, age_value + response_delay)`, warn
  once per origin above 60 s of skew. Every test injects the clock; none calls `time.Now()`.
- **Collapse + client disconnect**: the classic bug is the leader's context dying with its client. The leader runs on
  a root-derived context with `COLLAPSE_LEAD_TIMEOUT`; the caller's context only decides whether *that* caller keeps
  waiting. Step 8's regression test is exactly "first caller cancels, 99 others still get bytes".
- **PGO regressions**: perf-lab's `hashChain` lesson - PGO inlined `sha256.Sum256` into a caller where the digest
  escaped: 10k allocs/req and -83 % on one route. edgecache hashes every request, so `Key` is `[32]byte`, `Secondary`
  uses a pooled scratch buffer, step 2 asserts 0 allocs, and `pgo.ps1` runs A/B/A/B interleaved - a route regressing
  beyond a round's noise means the profile is rejected, not committed.
- **Correctness beats hit ratio**: every deviation from the RFC is a named config key defaulting to spec-compliant
  (`CACHE_AUTHORIZATION=false`, `DEFAULT_SWR=0s`), and `summary.md` records the settings used for the run.

## Open questions
1. `PURGE` on the proxy port at all, or is `POST /admin/purge` on loopback enough (one fewer auth surface)? Assumed
   both, PURGE disabled unless `EDGECACHE_PURGE_TOKEN` is set.
2. Disk cache survives restarts by default (scan recovery) or starts empty for predictable benchmark numbers? Assumed
   recover, with `-Fresh` in `scripts/cache.ps1` wiping the dir.
3. Persist the tag index, or accept that surrogate purges only cover entries whose metas were scanned at boot? Assumed
   rebuild from metas during recovery, no separate file.
4. Schedule stream-sharing (alternative 3) as a P16 follow-up now, or only if the cold-cache p99 says so?
