<#
.SYNOPSIS
  Collapse-streaming A/B: a thundering herd on cold large objects, stream off vs on.
.DESCRIPTION
  Builds edgecache, slow-origin and herd, starts slow-origin with a paced body
  (-BodyBytes at -Rate bytes/s, so one download takes a known time), then runs
  -Pairs A/B pairs in ABBA order. Every run starts a fresh edgecache (empty memory
  tier) with EDGECACHE_COLLAPSE_STREAM_BYTES=0 (off) or -StreamBytes (on) and runs
  `herd`: -Rounds cold keys, -N concurrent GETs each. The leader is the X-Cache: MISS
  request, the waiters are the collapsed HITs.

  Output: results/herd/<ts>/{off,on}-<i>.json (raw herd reports), summary.md (one row
  per run plus the median per mode).
.EXAMPLE
  pwsh -NoProfile -File scripts/herd.ps1
  pwsh -NoProfile -File scripts/herd.ps1 -Pairs 2 -N 16 -BodyBytes 1048576 -Rate 4194304
#>
param(
    [int]$Pairs = 3,
    [int]$N = 32,
    [int]$Rounds = 10,
    [int]$BodyBytes = 4MB,
    [int]$Rate = 8MB,
    [string]$StreamBytes = '8MiB',
    [int]$ProxyPort = 8081,
    [int]$AdminPort = 9101,
    [int]$OriginPort = 8082
)
. (Join-Path $PSScriptRoot '_lib.ps1')

Stop-StrayServers
foreach ($p in @($ProxyPort, $AdminPort, $OriginPort)) {
    if (Test-PortBusy -P $p) { throw "port $p is busy" }
}
$edge = Build-Edgecache
$originExe = Build-Edgecache -Package './cmd/slow-origin' -Name 'slow-origin'
$herdExe = Build-Edgecache -Package './cmd/herd' -Name 'herd'
$out = New-ResultDir -Experiment 'herd'

$originEnv = @{
    ORIGIN_ADDR          = "127.0.0.1:$OriginPort"
    ORIGIN_LATENCY       = '20ms'
    ORIGIN_JITTER        = '0s'
    ORIGIN_BODY_BYTES    = "$BodyBytes"
    ORIGIN_RATE          = "$Rate"
    ORIGIN_CACHE_CONTROL = 'max-age=600'
    ORIGIN_KEYSPACE      = '100000'
}
$origin = Start-Server -Exe $originExe -HealthUrl "http://127.0.0.1:$OriginPort/obj/0" -Env $originEnv -LogFile (Join-Path $out 'origin.log')
$rows = @()
try {
    $run = 0
    for ($i = 1; $i -le $Pairs; $i++) {
        $order = if ($i % 2 -eq 1) { @('off', 'on') } else { @('on', 'off') }
        foreach ($mode in $order) {
            $sb = if ($mode -eq 'on') { $StreamBytes } else { '0' }
            $edgeEnv = @{
                EDGECACHE_COLLAPSE_STREAM_BYTES = $sb
                EDGECACHE_MEM_MAX_OBJECT_BYTES  = '64MiB'
            }
            $p = Start-Edgecache -Exe $edge -Origin "http://127.0.0.1:$OriginPort" -Port $ProxyPort -AdminPort $AdminPort -Env $edgeEnv -LogFile (Join-Path $out "edge-$mode-$i.log")
            try {
                $json = Join-Path $out "$mode-$i.json"
                $start = 1000 + $run * ($Rounds + 2)
                Write-Host ("[{0}/{1}] stream {2}: n={3} rounds={4} body={5} rate={6}/s" -f ($run + 1), ($Pairs * 2), $mode, $N, $Rounds, $BodyBytes, $Rate) -ForegroundColor Cyan
                & $herdExe -base "http://127.0.0.1:$ProxyPort/obj/" -n $N -rounds $Rounds -start $start -bytes $BodyBytes -label "$mode-$i" -json $json
                if ($LASTEXITCODE -ne 0) { throw "herd failed ($LASTEXITCODE)" }
                $rep = Get-Content $json -Raw | ConvertFrom-Json
                foreach ($st in $rep.stats) {
                    $rows += [pscustomobject]@{ mode = $mode; run = $i; role = $st.role; count = $st.count
                        ttfb50 = $st.ttfb_p50_ms; ttfb99 = $st.ttfb_p99_ms; tot50 = $st.total_p50_ms; tot99 = $st.total_p99_ms }
                }
            } finally {
                Stop-Server -Process $p
            }
            $run++
        }
    }
} finally {
    Stop-Server -Process $origin
    Get-EventSubscriber -ErrorAction SilentlyContinue | Unregister-Event -ErrorAction SilentlyContinue
}

function Get-Median([double[]]$v) {
    $s = @($v | Sort-Object)
    if ($s.Count -eq 0) { return [double]::NaN }
    if ($s.Count % 2 -eq 1) { return $s[[int][math]::Floor($s.Count / 2)] }
    return ($s[$s.Count / 2 - 1] + $s[$s.Count / 2]) / 2
}

$downloadMs = [math]::Round($BodyBytes / $Rate * 1000)
$lines = @(
    '# Collapse streaming: herd on cold objects, stream off vs on',
    '',
    (Get-MachineHeader),
    '',
    "- slow-origin: $BodyBytes-byte body paced at $Rate bytes/s (~$downloadMs ms per download), 20 ms header latency, max-age=600",
    "- edgecache: memory tier only (EDGECACHE_MEM_MAX_OBJECT_BYTES=64MiB), fresh process per run; on = EDGECACHE_COLLAPSE_STREAM_BYTES=$StreamBytes, off = 0",
    "- herd: $N concurrent GETs per cold key, $Rounds keys per run, $Pairs ABBA pairs; leader = X-Cache MISS, waiter = collapsed HIT",
    '',
    '## Median of the per-run percentiles',
    '',
    '| stream | role | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |',
    '|---|---|---:|---:|---:|---:|'
)
foreach ($mode in @('off', 'on')) {
    foreach ($role in @('leader', 'waiter')) {
        $r = @($rows | Where-Object { $_.mode -eq $mode -and $_.role -eq $role })
        if ($r.Count -eq 0) { continue }
        $lines += ('| {0} | {1} | {2} | {3} | {4} | {5} |' -f $mode, $role,
            (Format-Num (Get-Median $r.ttfb50) '0.0'), (Format-Num (Get-Median $r.ttfb99) '0.0'),
            (Format-Num (Get-Median $r.tot50) '0.0'), (Format-Num (Get-Median $r.tot99) '0.0'))
    }
}
$lines += @('', '## Every run', '', '| stream | pair | role | requests | ttfb p50 ms | ttfb p99 ms | total p50 ms | total p99 ms |', '|---|---:|---|---:|---:|---:|---:|---:|')
foreach ($r in $rows) {
    $lines += ('| {0} | {1} | {2} | {3} | {4} | {5} | {6} | {7} |' -f $r.mode, $r.run, $r.role, $r.count,
        (Format-Num $r.ttfb50 '0.0'), (Format-Num $r.ttfb99 '0.0'), (Format-Num $r.tot50 '0.0'), (Format-Num $r.tot99 '0.0'))
}
$summary = Join-Path $out 'summary.md'
Set-Content -Path $summary -Value $lines -Encoding utf8
Write-Host ''
Get-Content $summary | Write-Host
Write-Host "`nwrote $summary" -ForegroundColor Green
