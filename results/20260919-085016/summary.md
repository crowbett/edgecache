# cache

_2026-09-19 08:52 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- tag: (none)
- edgecache: `C:\Users\corbe\dev\go\edgecache\bin\edgecache.exe` (mem 256MiB, disk disabled, env: (none))
- slow-origin: `C:\Users\corbe\dev\go\edgecache\bin\slow-origin.exe` (50ms +/- 10ms, 16384 B bodies, `Cache-Control: max-age=60`, keyspace 1000)
- oha: -c 64 -z 30s per mode (warmup 3s, discarded, for warm/no-cache)
- distribution: zipf(s=1) over 1000 keys via `--urls-from-file` (20000 URLs, 984 distinct; oha oha 1.16.0)
- hit ratio = (hit+stale+revalidated)/all from `edgecache_requests_total` deltas; origin rps from `edgecache_origin_requests_total` deltas (the origin row reports oha rps)

| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 57202 | 0.56 | 2.35 | 8.65 | 99.9% | 33 | 1.0000 |
| warm | 48517 | 0.60 | 2.69 | 11.87 | 100.0% | 33 | 1.0000 |
| no-cache | 1049 | 55.96 | 89.53 | 111.85 | 100.0% | 1047 | 1.0000 |
| origin | 1267 | 50.56 | 58.54 | 60.42 | n/a | 1267 | 1.0000 |

| mode | hit | miss | stale | revalidated | bypass | error | collapsed |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold | 1715472 | 982 | 0 | 0 | 0 | 0 | 301 |
| warm | 1454715 | 0 | 0 | 984 | 0 | 0 | 462 |
| no-cache | 0 | 0 | 0 | 31434 | 0 | 10 | 10991 |
| origin | 0 | 0 | 0 | 6 | 0 | 0 | 0 |

_total wall time: 2.2 min_
