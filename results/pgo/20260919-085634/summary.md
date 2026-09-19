# pgo

_2026-09-19 09:01 | AMD Ryzen 7 3700X 8-Core Processor | go1.27.0 windows/amd64 | Microsoft Windows NT 10.0.26200.0 | PowerShell 7.6.6_

- profile: 30 s CPU under warm load (zipf via --urls-from-file (1000 keys), c=64) -> `cmd/edgecache/default.pgo` (216320 bytes)
- pgo binary: `C:\Users\corbe\dev\go\edgecache\bin\edgecache-pgo.exe` (-pgo=default.pgo, 19417088 bytes)
- nopgo binary: `C:\Users\corbe\dev\go\edgecache\bin\edgecache-nopgo.exe` (-pgo=off, 19290112 bytes)
- comparison: 3 round(s) of nopgo/pgo interleaved, cache.ps1 -Modes warm, oha -c 64 -z 30s (warmup 3s), values averaged
- edgecache: mem 256MiB, disk disabled, env: (none)

| mode | rps nopgo | rps pgo | Δ% | p50 nopgo ms | p50 pgo ms | p99 nopgo ms | p99 pgo ms | p99 Δ% | hit nopgo | hit pgo |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| warm | 61605 | 63507 | +3.1 | 0.56 | 0.56 | 7.80 | 7.06 | -9.4 | 100.0% | 100.0% |

| round | rps nopgo | rps pgo | p99 nopgo ms | p99 pgo ms |
|---|---:|---:|---:|---:|
| 1 | 53861 | 58086 | 9.91 | 8.18 |
| 2 | 64807 | 65616 | 6.85 | 6.54 |
| 3 | 66148 | 66820 | 6.63 | 6.47 |

_round-to-round rps spread: nopgo 19.9%, pgo 13.8%; a Δ% inside that spread is noise, not a result._
_total wall time: 4.9 min. Per-round tables: warm-<side>-<n>/summary.md_
