# cache

_2026-09-22 18:16 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- tag: (none)
- edgecache: `<tmp>\q7bench\edgecache-after.exe` (mem 256MiB, disk disabled, env: (none))
- slow-origin: `<tmp>\q7bench\slow-origin.exe` (50ms +/- 10ms, 16384 B bodies, `Cache-Control: max-age=60`, keyspace 1000)
- oha: -c 64 -z 30s per mode (warmup 3s, discarded, for warm/no-cache)
- distribution: zipf(s=1) over 1000 keys via `--urls-from-file` (20000 URLs, 984 distinct; oha oha 1.16.0)
- hit ratio = (hit+stale+revalidated)/all from `edgecache_requests_total` deltas; origin rps from `edgecache_origin_requests_total` deltas (the origin row reports oha rps)

| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 39253 | 0.65 | 3.67 | 14.50 | 99.9% | 33 | 1.0000 |
| warm | 36328 | 0.68 | 4.01 | 16.12 | 100.0% | 33 | 1.0000 |
| no-cache | 1063 | 55.32 | 88.15 | 107.37 | 100.0% | 1063 | 1.0000 |
| origin | 1265 | 50.59 | 58.57 | 60.50 | n/a | 1265 | 1.0000 |

| mode | hit | miss | stale | revalidated | bypass | error | collapsed |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 1177383 | 984 | 0 | 0 | 0 | 0 | 291 |
| warm | 1089371 | 0 | 0 | 983 | 0 | 0 | 314 |
| no-cache | 0 | 0 | 0 | 31898 | 0 | 10 | 11116 |
| origin | 0 | 0 | 0 | 0 | 0 | 0 | 0 |

_total wall time: 2.2 min_
