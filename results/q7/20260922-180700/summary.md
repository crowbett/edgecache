# Q7 A/B: HEAD / Range / relative purge / trusted proxies

_2026-09-22 | AMD Ryzen 7 3700X (8c/16t) | go1.27.0 windows/amd64 | loaded box: other agents building and testing in parallel_

before = `80898e9` (git worktree), after = the Q7 working tree (same as `dfa1180` + README/bench file). Both built with `go build -trimpath` (default.pgo picked up by -pgo=auto), same slow-origin binary.

## oha warm, ABAB x6

`scripts/cache.ps1 -Modes warm -Duration 15s` (c=64, 3 s discarded warmup, zipf(s=1) over 1000 keys, 16 KiB, max-age=60, 50 ms +/- 10 ms origin, 256 MiB memory tier), rounds interleaved before/after.

| round | rps before | rps after | p50 before | p50 after | p99 before | p99 after | hit before | hit after |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 32339 | 25674 | 0.80 | 0.85 | 17.09 | 25.69 | 100.0% | 100.0% |
| 2 | 35393 | 28809 | 0.76 | 0.77 | 15.42 | 22.01 | 100.0% | 100.0% |
| 3 | 26814 | 30771 | 0.82 | 0.80 | 23.22 | 19.39 | 100.0% | 100.0% |
| 4 | 24218 | 22820 | 0.80 | 0.86 | 31.81 | 30.77 | 100.0% | 100.0% |
| 5 | 27761 | 29696 | 0.82 | 0.76 | 22.75 | 21.41 | 100.0% | 100.0% |
| 6 | 34496 | 39549 | 0.73 | 0.70 | 16.36 | 12.74 | 100.0% | 100.0% |

| median | rps | p50 ms | p99 ms |
|---|---:|---:|---:|
| before | 30050 | 0.80 | 19.92 |
| after | 29252 | 0.79 | 21.71 |
| delta | -2.7% | -1.9% | +9.0% |

rps spread (max-min)/median: before 37%, after 57%; p99 spread: before 82%, after 83%

## Microbenchmarks, ABAB x8 (benchstat)

Test binaries built once per side (`go test -c`, CGO_ENABLED=0), then 8 rounds alternating before/after, `-benchmem`, default benchtime. `BenchmarkHandlerHit` (new, `internal/cache/handler_bench_test.go`) was copied into the before worktree so both sides run the same code.

```
goos: windows
goarch: amd64
pkg: github.com/crowbett/edgecache/internal/bench
cpu: AMD Ryzen 7 3700X 8-Core Processor             
           │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
           │                                                             sec/op                                                             │                                                    sec/op                                                     vs base         │
HitPath-16                                                                                                                     3.298µ ± 11%                                                                                                   3.202µ ± 48%  ~ (p=0.574 n=8)

           │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
           │                                                              B/s                                                               │                                                     B/s                                                       vs base         │
HitPath-16                                                                                                                    4.626Gi ± 13%                                                                                                  4.768Gi ± 20%  ~ (p=0.574 n=8)

           │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
           │                                                              B/op                                                              │                                                    B/op                                                     vs base           │
HitPath-16                                                                                                                       368.0 ± 0%                                                                                                   368.0 ± 0%  ~ (p=1.000 n=8) ¹
¹ all samples are equal

           │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
           │                                                           allocs/op                                                            │                                                 allocs/op                                                   vs base           │
HitPath-16                                                                                                                       4.000 ± 0%                                                                                                   4.000 ± 0%  ~ (p=1.000 n=8) ¹
¹ all samples are equal

pkg: github.com/crowbett/edgecache/internal/cache
                    │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
                    │                                                             sec/op                                                             │                                                 sec/op                                                   vs base              │
HandlerHit/plain-16                                                                                                                     6.951µ ± 52%                                                                                              7.012µ ± 49%       ~ (p=0.382 n=8)
HandlerHit/xfp-16                                                                                                                       6.855µ ± 52%                                                                                              7.173µ ± 50%       ~ (p=0.574 n=8)
geomean                                                                                                                                 6.903µ                                                                                                    7.092µ        +2.74%

                    │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
                    │                                                              B/s                                                               │                                                   B/s                                                    vs base              │
HandlerHit/plain-16                                                                                                                    2.196Gi ± 30%                                                                                             2.178Gi ± 19%       ~ (p=0.382 n=8)
HandlerHit/xfp-16                                                                                                                      2.229Gi ± 29%                                                                                             2.127Gi ± 18%       ~ (p=0.574 n=8)
geomean                                                                                                                                2.212Gi                                                                                                   2.152Gi        -2.71%

                    │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
                    │                                                              B/op                                                              │                                                  B/op                                                    vs base              │
HandlerHit/plain-16                                                                                                                     1.375Ki ± 0%                                                                                              1.375Ki ± 0%       ~ (p=0.786 n=8)
HandlerHit/xfp-16                                                                                                                       1.376Ki ± 0%                                                                                              1.375Ki ± 0%       ~ (p=0.601 n=8)
geomean                                                                                                                                 1.375Ki                                                                                                   1.375Ki       -0.02%

                    │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/before.txt │ C:/Users/corbe/AppData/Local/Temp/claude/C--Users-corbe-dev/4e24afc9-6e74-4025-8652-aa698c96c633/scratchpad/q7bench/after.txt │
                    │                                                           allocs/op                                                            │                                               allocs/op                                                vs base                │
HandlerHit/plain-16                                                                                                                       25.00 ± 0%                                                                                              25.00 ± 0%       ~ (p=1.000 n=8) ¹
HandlerHit/xfp-16                                                                                                                         25.00 ± 0%                                                                                              25.00 ± 0%       ~ (p=1.000 n=8) ¹
geomean                                                                                                                                   25.00                                                                                                   25.00       +0.00%
¹ all samples are equal
```
