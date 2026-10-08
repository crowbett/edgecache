# Collapse streaming: herd on cold objects, stream off vs on

_2026-10-07 19:17 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- slow-origin: 4194304-byte body paced at 8388608 bytes/s (~500 ms per download), 20 ms header latency, max-age=600
- edgecache: memory tier only (EDGECACHE_MEM_MAX_OBJECT_BYTES=64MiB), fresh process per run; on = EDGECACHE_COLLAPSE_STREAM_BYTES=8MiB, off = 0
- herd: 32 concurrent GETs per cold key, 10 keys per run, 3 ABBA pairs; leader = X-Cache MISS, waiter = collapsed HIT

## Median of the per-run percentiles

| stream | role | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |
|---|---|---:|---:|---:|---:|
| off | leader | 24.3 | 39.7 | 522.2 | 529.3 |
| off | waiter | 539.2 | 582.2 | 567.8 | 611.2 |
| on | leader | 25.2 | 30.5 | 521.7 | 525.8 |
| on | waiter | 25.6 | 36.9 | 522.2 | 528.5 |

## Every run

| stream | pair | role | requests | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |
|---|---:|---|---:|---:|---:|---:|---:|
| off | 1 | leader | 10 | 28.5 | 39.7 | 524.5 | 529.3 |
| off | 1 | waiter | 310 | 540.1 | 585.5 | 573.3 | 611.2 |
| on | 1 | leader | 10 | 24.9 | 30.5 | 521.7 | 525.8 |
| on | 1 | waiter | 310 | 25.6 | 36.9 | 522.2 | 528.5 |
| on | 2 | leader | 10 | 25.2 | 36.2 | 521.7 | 527.6 |
| on | 2 | waiter | 310 | 25.6 | 37.3 | 522.6 | 529.1 |
| off | 2 | leader | 10 | 24.3 | 26.6 | 522.2 | 526.8 |
| off | 2 | waiter | 310 | 539.2 | 570.2 | 567.8 | 602.4 |
| off | 3 | leader | 10 | 24.2 | 42.3 | 521.0 | 529.3 |
| off | 3 | waiter | 310 | 536.4 | 582.2 | 565.1 | 674.3 |
| on | 3 | leader | 10 | 25.2 | 27.3 | 521.6 | 522.7 |
| on | 3 | waiter | 310 | 25.5 | 36.8 | 521.9 | 524.4 |
