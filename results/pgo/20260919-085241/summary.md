# pgo

_2026-09-19 08:56 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- profile: 30 s CPU under warm load (zipf via --urls-from-file (1000 keys), c=64) -> `cmd/edgecache/default.pgo` (230021 bytes)
- pgo binary: `bin\edgecache-pgo.exe` (-pgo=default.pgo, 19478528 bytes)
- nopgo binary: `bin\edgecache-nopgo.exe` (-pgo=off, 19290112 bytes)
- comparison: 2 round(s) of nopgo/pgo interleaved, cache.ps1 -Modes warm, oha -c 64 -z 30s (warmup 3s), values averaged
- edgecache: mem 256MiB, disk disabled, env: (none)

| mode | rps nopgo | rps pgo | Δ% | p50 nopgo ms | p50 pgo ms | p99 nopgo ms | p99 pgo ms | p99 Δ% | hit nopgo | hit pgo |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| warm | 43910 | 56923 | +29.6 | 0.61 | 0.58 | 19.11 | 8.91 | -53.4 | 100.0% | 100.0% |

| round | rps nopgo | rps pgo | p99 nopgo ms | p99 pgo ms |
|---|---:|---:|---:|---:|
| 1 | 23667 | 60985 | 31.12 | 7.71 |
| 2 | 64153 | 52860 | 7.11 | 10.10 |

_round-to-round rps spread: nopgo 92.2%, pgo 14.3%; a Δ% inside that spread is noise, not a result._
_total wall time: 3.6 min. Per-round tables: warm-<side>-<n>/summary.md_
