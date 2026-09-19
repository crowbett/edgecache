<#
.SYNOPSIS
  Profile-guided optimisation pipeline for edgecache: collect default.pgo under warm cache
  load, build with/without PGO, run cache.ps1 (warm mode) A/B/A/B, print the delta.

.DESCRIPTION
  1. Build bin/edgecache-prof.exe (symbolised, -pgo=off) and bin/slow-origin.exe; start both.
  2. Warm the cache, then capture one admin /debug/pprof/profile?seconds=<Seconds> while oha
     keeps the warm load on for Seconds+1 s. Write it to cmd/edgecache/default.pgo, which
     `go build` picks up automatically (-pgo=auto). default.pgo is committed.
  3. Build bin/edgecache-pgo.exe (default.pgo auto-applied) and bin/edgecache-nopgo.exe
     (-pgo=off) and verify via `go version -m` that only the former carries -pgo.
  4. Run cache.ps1 -Modes warm against both, interleaved nopgo/pgo per round (-Rounds,
     default 2) so machine drift hits both sides equally; average the rounds.
  5. Write results/pgo/<yyyyMMdd-HHmmss>/summary.md: rps nopgo | rps pgo | delta% | p99
     nopgo | p99 pgo | hit ratio both sides, plus default.pgo size, and print it. Per-round
     cache.ps1 results are in warm-<nopgo|pgo>-<n>/ under the same directory.

  A regression beyond a round's noise means the profile is rejected, not committed
  (docs/PLAN.md "PGO regressions"): the script prints the table, the human decides.

  Works on Windows PowerShell 5.1 and pwsh 7. Requires: go, oha on PATH.

.EXAMPLE
  pwsh -NoProfile -File scripts/pgo.ps1
  # 30 s profile under warm load, 2 x (30 s nopgo, 30 s pgo)

.EXAMPLE
  pwsh -NoProfile -File scripts/pgo.ps1 -Seconds 10 -Duration 5s -Rounds 1
  # quick smoke run

.EXAMPLE
  pwsh -NoProfile -File scripts/pgo.ps1 -Rounds 3 -Connections 128 -Distribution uniform
#>
[CmdletBinding()]
param(
    [int]$Seconds = 30,
    [int]$Connections = 64,
    [string]$Duration = '30s',
    [string]$Warmup = '3s',
    [int]$Rounds = 2,
    [ValidateSet('auto', 'zipf', 'uniform')] [string]$Distribution = 'auto',
    [int]$Keyspace = 1000,
    [string]$MemBytes = '256MiB',
    [string]$DiskDir = '',
    [hashtable]$Env = @{},
    [int]$Port = 8081,
    [int]$AdminPort = 9101,
    [int]$OriginPort = 8082
)

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

. (Join-Path $PSScriptRoot '_lib.ps1')

$pgoPath = Join-Path $script:RepoRoot 'cmd\edgecache\default.pgo'
$cacheScript = Join-Path $PSScriptRoot 'cache.ps1'

# ---- local helpers -------------------------------------------------------------------------

# Invoke-CacheScript runs cache.ps1 -Modes warm for one binary and returns its warm row.
function Invoke-CacheScript {
    param([string]$ExePath, [string]$OriginExe, [string]$Tag, [string]$OutDir)
    & $cacheScript -Exe $ExePath -OriginExe $OriginExe -Modes warm -Quiet -Tag $Tag -OutDir $OutDir `
        -Connections $Connections -Duration $Duration -Warmup $Warmup -Distribution $Distribution -Keyspace $Keyspace `
        -MemBytes $MemBytes -DiskDir $DiskDir -Env $Env -Port $Port -AdminPort $AdminPort -OriginPort $OriginPort | Out-Null
    $code = $LASTEXITCODE
    $summary = Join-Path $OutDir 'summary.json'
    if (-not (Test-Path $summary)) { throw "cache.ps1 ($Tag) produced no summary.json (exit $code)" }
    if ($code -ne 0) { throw "cache.ps1 ($Tag) failed with exit code $code; see $OutDir\summary.md" }
    $rows = @(Get-Content $summary -Raw | ConvertFrom-Json)
    $row = @($rows | Where-Object { $_.mode -eq 'warm' })
    if ($row.Count -eq 0) { throw "cache.ps1 ($Tag) has no warm row" }
    return $row[0]
}

function Get-Avg {
    param([object[]]$Rows, [string]$Field)
    $vals = @()
    foreach ($r in $Rows) {
        $v = [double]$r.$Field
        if (-not [double]::IsNaN($v)) { $vals += $v }
    }
    if ($vals.Count -eq 0) { return [double]::NaN }
    return (($vals | Measure-Object -Average).Average)
}

# ---- preflight -----------------------------------------------------------------------------

if ($Seconds -lt 5) { throw '-Seconds must be >= 5' }
if ($Rounds -lt 1) { throw '-Rounds must be >= 1' }
Assert-Tool -Name 'go' -Install 'winget install GoLang.Go'
Assert-Tool -Name 'oha' -Install 'winget install hatoo.oha / cargo install oha'
Stop-StrayServers
foreach ($pt in @($Port, $AdminPort, $OriginPort)) {
    if (Test-PortBusy -P $pt) { throw "port 127.0.0.1:$pt is already in use; stop whatever is listening there and retry" }
}

$dir = New-ResultDir -Experiment 'pgo'
$proxyBase = "http://127.0.0.1:$Port"
$originBase = "http://127.0.0.1:$OriginPort"
$profileUrl = "http://127.0.0.1:$AdminPort/debug/pprof/profile?seconds=$Seconds"
$originProc = $null
$cacheProc = $null
$client = $null
$sw = [System.Diagnostics.Stopwatch]::StartNew()

$useZipf = ($Distribution -eq 'zipf') -or (($Distribution -eq 'auto') -and (Test-OhaUrlsFromFile))
if ($useZipf) {
    $urlList = Join-Path $dir 'urls-proxy.txt'
    $null = New-ZipfUrlList -Base $proxyBase -Path $urlList -Keyspace $Keyspace
    $distNote = "zipf via --urls-from-file ($Keyspace keys)"
} else {
    $urlRegex = Get-UniformRegexUrl -Base $proxyBase -Keyspace $Keyspace
    $distNote = "uniform via --rand-regex-url '$urlRegex'"
}
function Invoke-WarmLoad {
    param([string]$Dur, [string]$RawOut = '')
    if ($useZipf) { return Invoke-Oha -UrlsFile $urlList -Connections $Connections -Duration $Dur -RawOut $RawOut }
    return Invoke-Oha -RandRegexUrl $urlRegex -Connections $Connections -Duration $Dur -RawOut $RawOut
}

# ---- 1+2. collect the profile --------------------------------------------------------------

Write-Host '[1/5] building bin/edgecache-prof.exe (symbolised, -pgo=off) and bin/slow-origin.exe ...' -ForegroundColor Cyan
$profExe = Build-Edgecache -Name 'edgecache-prof' -Pgo 'off' -Ldflags ''
$originExe = Build-Edgecache -Package './cmd/slow-origin' -Name 'slow-origin'

try {
    Write-Host "[2/5] collecting $Seconds s CPU profile under warm load ($distNote, c=$Connections) ..." -ForegroundColor Cyan
    $originProc = Start-SlowOrigin -Exe $originExe -Port $OriginPort -Keyspace $Keyspace -LogFile (Join-Path $dir 'origin-prof.log')
    $cacheProc = Start-Edgecache -Exe $profExe -Origin $originBase -Port $Port -AdminPort $AdminPort -MemBytes $MemBytes -DiskDir $DiskDir -Env $Env -LogFile (Join-Path $dir 'edgecache-prof.log')

    Write-Host "      warmup $Warmup ..." -ForegroundColor DarkGray
    $null = Invoke-WarmLoad -Dur $Warmup

    Add-Type -AssemblyName System.Net.Http
    $client = New-Object System.Net.Http.HttpClient
    $client.Timeout = [TimeSpan]::FromSeconds($Seconds + 90)
    $task = $client.GetByteArrayAsync($profileUrl)
    Start-Sleep -Milliseconds 500
    if ($task.IsFaulted) { throw "profile request failed early: $($task.Exception.GetBaseException().Message)" }

    # Load covers the whole profiling window (+1 s so the profiler stops under load).
    $before = Get-Metrics -Url "http://127.0.0.1:$AdminPort/metrics"
    $j = Invoke-WarmLoad -Dur "$($Seconds + 1)s" -RawOut (Join-Path $dir 'collect.json')
    $after = Get-Metrics -Url "http://127.0.0.1:$AdminPort/metrics"
    $o = ConvertFrom-OhaResult -Oha $j
    $c = Get-CacheStats -Delta (Get-MetricsDelta -Before $before -After $after) -Seconds $o.seconds
    Write-Host ("      collection load: {0} rps, hit ratio {1}, status {2}" -f (Format-Num $o.rps '0'), (Format-Ratio $c.hit_ratio), $o.status) -ForegroundColor DarkGray
    if ($o.success -lt 0.999) { Write-Warning "non-2xx responses during collection ($($o.status)); the profile includes the error path" }

    Write-Host '      waiting for the profiler to finish ...' -ForegroundColor DarkGray
    $bytes = $task.GetAwaiter().GetResult()
    if ($bytes.Length -lt 100) { throw "profile response is only $($bytes.Length) bytes; is pprof serving on the admin listener :$AdminPort ?" }
    [System.IO.File]::WriteAllBytes($pgoPath, $bytes)
    Copy-Item $pgoPath (Join-Path $dir 'default.pgo')
    Write-Host ("      wrote {0} ({1} bytes)" -f $pgoPath, $bytes.Length) -ForegroundColor Green
} finally {
    if ($client) { $client.Dispose() }
    if ($cacheProc) { Stop-Server -Process $cacheProc; $cacheProc = $null }
    if ($originProc) { Stop-Server -Process $originProc; $originProc = $null }
}

# ---- 3. build both -------------------------------------------------------------------------

Write-Host '[3/5] building bin/edgecache-pgo.exe (default.pgo auto) and bin/edgecache-nopgo.exe (-pgo=off) ...' -ForegroundColor Cyan
$pgoExe = Build-Edgecache -Name 'edgecache-pgo'
$nopgoExe = Build-Edgecache -Name 'edgecache-nopgo' -Pgo 'off'
$pgoSetting = Get-PgoBuildSetting -Exe $pgoExe
$nopgoSetting = Get-PgoBuildSetting -Exe $nopgoExe
if (-not $pgoSetting) { throw "bin/edgecache-pgo.exe carries no -pgo build setting; default.pgo was not applied (is it at $pgoPath ?)" }
if ($nopgoSetting) { throw "bin/edgecache-nopgo.exe unexpectedly carries -pgo=$nopgoSetting" }
Write-Host "      pgo: -pgo=$pgoSetting | nopgo: (none)" -ForegroundColor DarkGray
Write-Host ("      sizes: pgo {0} bytes, nopgo {1} bytes" -f (Get-Item $pgoExe).Length, (Get-Item $nopgoExe).Length) -ForegroundColor DarkGray

# ---- 4. A/B/A/B -----------------------------------------------------------------------------

$runs = @{ nopgo = @(); pgo = @() }
for ($r = 1; $r -le $Rounds; $r++) {
    foreach ($side in @('nopgo', 'pgo')) {
        $exe = $nopgoExe
        if ($side -eq 'pgo') { $exe = $pgoExe }
        Write-Host ("[4/5] round {0}/{1}: {2}  (warm, {3} + {4} warmup)" -f $r, $Rounds, $side, $Duration, $Warmup) -ForegroundColor Cyan
        $row = Invoke-CacheScript -ExePath $exe -OriginExe $originExe -Tag "$side-r$r" -OutDir (Join-Path $dir "warm-$side-$r")
        Write-Host ("      {0} rps, p99 {1} ms, hit ratio {2}" -f (Format-Num $row.rps '0'), (Format-Num $row.p99_ms), (Format-Ratio ([double]$row.hit_ratio))) -ForegroundColor DarkGray
        $runs[$side] += , $row
    }
}

# ---- 5. aggregate + write ------------------------------------------------------------------

$rpsA = Get-Avg -Rows $runs['nopgo'] -Field 'rps'
$rpsB = Get-Avg -Rows $runs['pgo'] -Field 'rps'
$p99A = Get-Avg -Rows $runs['nopgo'] -Field 'p99_ms'
$p99B = Get-Avg -Rows $runs['pgo'] -Field 'p99_ms'
$p50A = Get-Avg -Rows $runs['nopgo'] -Field 'p50_ms'
$p50B = Get-Avg -Rows $runs['pgo'] -Field 'p50_ms'
$hitA = Get-Avg -Rows $runs['nopgo'] -Field 'hit_ratio'
$hitB = Get-Avg -Rows $runs['pgo'] -Field 'hit_ratio'
$delta = [double]::NaN
if ($rpsA -gt 0) { $delta = ($rpsB - $rpsA) / $rpsA * 100 }
$p99Delta = [double]::NaN
if ($p99A -gt 0) { $p99Delta = ($p99B - $p99A) / $p99A * 100 }

# Per-round spread: the noise floor a delta has to clear before it means anything.
$rpsSpreadA = 0.0; $rpsSpreadB = 0.0
if ($Rounds -gt 1) {
    $a = @($runs['nopgo'] | ForEach-Object { [double]$_.rps }); $b = @($runs['pgo'] | ForEach-Object { [double]$_.rps })
    $rpsSpreadA = (($a | Measure-Object -Maximum).Maximum - ($a | Measure-Object -Minimum).Minimum) / $rpsA * 100
    $rpsSpreadB = (($b | Measure-Object -Maximum).Maximum - ($b | Measure-Object -Minimum).Minimum) / $rpsB * 100
}

$table = [pscustomobject]@{
    mode          = 'warm'
    rps_nopgo     = [math]::Round($rpsA, 1)
    rps_pgo       = [math]::Round($rpsB, 1)
    delta_pct     = [math]::Round($delta, 2)
    p50_nopgo     = [math]::Round($p50A, 3)
    p50_pgo       = [math]::Round($p50B, 3)
    p99_nopgo     = [math]::Round($p99A, 3)
    p99_pgo       = [math]::Round($p99B, 3)
    p99_delta_pct = [math]::Round($p99Delta, 2)
    hit_nopgo     = $hitA
    hit_pgo       = $hitB
    rounds        = $Rounds
    spread_nopgo  = [math]::Round($rpsSpreadA, 2)
    spread_pgo    = [math]::Round($rpsSpreadB, 2)
}

$deltaHdr = [string][char]0x0394 + '%'
$pgoSize = (Get-Item $pgoPath).Length
$lines = @()
$lines += '# pgo'
$lines += ''
$lines += (Get-MachineHeader)
$lines += ''
$lines += "- profile: $Seconds s CPU under warm load ($distNote, c=$Connections) -> ``cmd/edgecache/default.pgo`` ($pgoSize bytes)"
$lines += "- pgo binary: ``$pgoExe`` (-pgo=$pgoSetting, $((Get-Item $pgoExe).Length) bytes)"
$lines += "- nopgo binary: ``$nopgoExe`` (-pgo=off, $((Get-Item $nopgoExe).Length) bytes)"
$lines += "- comparison: $Rounds round(s) of nopgo/pgo interleaved, cache.ps1 -Modes warm, oha -c $Connections -z $Duration (warmup $Warmup), values averaged"
$diskNote = 'disabled'
if ($DiskDir) { $diskNote = '`' + $DiskDir + '`' }
$lines += "- edgecache: mem $MemBytes, disk $diskNote, env: $(Format-EnvLine $Env)"
$lines += ''
$lines += "| mode | rps nopgo | rps pgo | $deltaHdr | p50 nopgo ms | p50 pgo ms | p99 nopgo ms | p99 pgo ms | p99 $deltaHdr | hit nopgo | hit pgo |"
$lines += '|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|'
$d = 'n/a'; if (-not [double]::IsNaN($table.delta_pct)) { $d = Format-Num $table.delta_pct '+0.0;-0.0;0.0' }
$pd = 'n/a'; if (-not [double]::IsNaN($table.p99_delta_pct)) { $pd = Format-Num $table.p99_delta_pct '+0.0;-0.0;0.0' }
$lines += ('| {0} | {1} | {2} | {3} | {4} | {5} | {6} | {7} | {8} | {9} | {10} |' -f $table.mode,
    (Format-Num $table.rps_nopgo '0'), (Format-Num $table.rps_pgo '0'), $d,
    (Format-Num $table.p50_nopgo), (Format-Num $table.p50_pgo), (Format-Num $table.p99_nopgo), (Format-Num $table.p99_pgo), $pd,
    (Format-Ratio $table.hit_nopgo), (Format-Ratio $table.hit_pgo))
$lines += ''
$lines += '| round | rps nopgo | rps pgo | p99 nopgo ms | p99 pgo ms |'
$lines += '|---|---:|---:|---:|---:|'
for ($r = 0; $r -lt $Rounds; $r++) {
    $ra = $runs['nopgo'][$r]; $rb = $runs['pgo'][$r]
    $lines += ('| {0} | {1} | {2} | {3} | {4} |' -f ($r + 1), (Format-Num ([double]$ra.rps) '0'), (Format-Num ([double]$rb.rps) '0'), (Format-Num ([double]$ra.p99_ms)), (Format-Num ([double]$rb.p99_ms)))
}
$lines += ''
if ($Rounds -gt 1) {
    $lines += "_round-to-round rps spread: nopgo $(Format-Num $table.spread_nopgo '0.0')%, pgo $(Format-Num $table.spread_pgo '0.0')%; a $deltaHdr inside that spread is noise, not a result._"
} else {
    $lines += '_one round only: no noise floor; rerun with -Rounds 2+ before trusting the delta._'
}
$lines += "_total wall time: $([math]::Round($sw.Elapsed.TotalMinutes, 1)) min. Per-round tables: warm-<side>-<n>/summary.md_"

$lines | Set-Content -Path (Join-Path $dir 'summary.md') -Encoding utf8
ConvertTo-Json -InputObject $table -Depth 4 | Set-Content -Path (Join-Path $dir 'summary.json') -Encoding utf8

Write-Host ''
$lines | ForEach-Object { Write-Host $_ }
Write-Host ''
Write-Host "results: $dir" -ForegroundColor Green
Write-Host "default.pgo: $pgoPath ($pgoSize bytes) -- commit it only if the table says PGO won" -ForegroundColor Green
exit 0
