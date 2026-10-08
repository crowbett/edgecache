# Collapse streaming: herd on cold objects, stream off vs on

_2026-10-07 19:32 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- slow-origin: 4194304-byte body paced at 8388608 bytes/s (~500 ms per download), 20 ms header latency, max-age=600
- edgecache: memory tier only (EDGECACHE_MEM_MAX_OBJECT_BYTES=64MiB), fresh process per run; on = EDGECACHE_COLLAPSE_STREAM_BYTES=8MiB, off = 0
- herd: 32 concurrent GETs per cold key, 10 keys per run, 3 ABBA pairs; leader = X-Cache MISS, waiter = collapsed HIT

## Median of the per-run percentiles

| stream | role | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |
|---|---|---:|---:|---:|---:|
| off | leader | 25.9 | 39.8 | 525.6 | 537.9 |
| off | waiter | 540.9 | 577.8 | 561.4 | 596.5 |
| on | leader | 27.4 | 48.5 | 522.6 | 533.1 |
| on | waiter | 26.4 | 52.5 | 522.7 | 534.1 |

## Every run

| stream | pair | role | requests | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |
|---|---:|---|---:|---:|---:|---:|---:|
| off | 1 | leader | 10 | 28.8 | 34.6 | 533.2 | 545.3 |
| off | 1 | waiter | 310 | 540.9 | 577.8 | 569.2 | 596.5 |
| on | 1 | leader | 10 | 27.9 | 52.1 | 523.3 | 533.1 |
| on | 1 | waiter | 310 | 29.1 | 53.1 | 524.5 | 534.1 |
| on | 2 | leader | 10 | 27.4 | 48.5 | 522.6 | 539.7 |
| on | 2 | waiter | 310 | 26.4 | 52.5 | 522.7 | 539.7 |
| off | 2 | leader | 10 | 25.7 | 39.8 | 523.0 | 530.7 |
| off | 2 | waiter | 310 | 539.6 | 581.3 | 559.8 | 616.1 |
| off | 3 | leader | 10 | 25.9 | 45.9 | 525.6 | 537.9 |
| off | 3 | waiter | 310 | 546.2 | 572.8 | 561.4 | 592.8 |
| on | 3 | leader | 10 | 24.4 | 31.4 | 521.0 | 525.2 |
| on | 3 | waiter | 310 | 24.0 | 33.7 | 521.2 | 525.7 |
