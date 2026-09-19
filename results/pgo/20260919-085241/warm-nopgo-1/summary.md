# cache

_2026-09-19 08:54 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- tag: nopgo-r1
- edgecache: `C:\Users\corbe\dev\go\edgecache\bin\edgecache-nopgo.exe` (mem 256MiB, disk disabled, env: (none))
- slow-origin: `C:\Users\corbe\dev\go\edgecache\bin\slow-origin.exe` (50ms +/- 10ms, 16384 B bodies, `Cache-Control: max-age=60`, keyspace 1000)
- oha: -c 64 -z 30s per mode (warmup 3s, discarded, for warm/no-cache)
- distribution: zipf(s=1) over 1000 keys via `--urls-from-file` (20000 URLs, 984 distinct; oha oha 1.16.0)
- hit ratio = (hit+stale+revalidated)/all from `edgecache_requests_total` deltas; origin rps from `edgecache_origin_requests_total` deltas (the origin row reports oha rps)

| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
|---|---:|---:|---:|---:|---:|---:|---:|
| warm | 23667 | 0.67 | 3.31 | 31.12 | 100.0% | 8 | 1.0000 |

| mode | hit | miss | stale | revalidated | bypass | error | collapsed |
|---|---:|---:|---:|---:|---:|---:|---:|
| warm | 709956 | 228 | 0 | 0 | 0 | 0 | 46 |

_total wall time: 0.6 min_
