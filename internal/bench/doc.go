// Package bench holds the hot-path microbenchmarks (go test -run '^$' -bench .
// -benchmem ./internal/bench/). It has no non-test code; the benchmarks run
// against the real packages (cachekey, rfc9111, memstore) and skip the ones
// whose implementation has not landed yet, so the numbers are always for
// production code, never for a mock.
//
// Targets (docs/PLAN.md "Benchmarks"): key build 0 allocs; memory hit round
// trip < 1 us and <= 4 allocs; the full hit path through a ResponseWriter < 2 us
// and <= 4 allocs; header copy < 500 ns. Baselines live in .bench/baseline.txt
// (scripts/bench.ps1 -Save baseline) and every perf change is compared with
// benchstat.
package bench
