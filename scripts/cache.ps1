<#
.SYNOPSIS
  Hit-ratio and latency run: oha against edgecache in front of slow-origin, one row per mode.

.DESCRIPTION
  Builds bin/edgecache.exe and bin/slow-origin.exe (unless -NoBuild / -Exe / -OriginExe),
  starts slow-origin (default 50 ms +/- 10 ms, 16 KiB bodies, max-age=60, keyspace 1000) on
  127.0.0.1:8082 and edgecache in front of it (proxy :8081, admin :9101, 256 MiB memory,
  disk tier only with -DiskDir), then runs oha for every -Modes entry:

    cold      first run on an empty cache, no warmup (with -DiskDir and no -Fresh the disk
              tier may already hold entries; the summary says so)
    warm      a discarded warmup run first, then the measured run
    no-cache  warm cache, clients send "Cache-Control: no-cache" (forced revalidation)
    origin    control: oha straight at slow-origin, no cache in the path

  URL distribution: when the installed oha has --urls-from-file (1.5+) the run uses a
  Zipf-weighted list over the keyspace (-Distribution zipf, the default when available),
  otherwise --rand-regex-url with a zero-padded [0-9]{d} path (value-uniform; paths look
  like /obj/007). The mode that ran is recorded in summary.md.

  Hit ratio and origin rps come from /metrics deltas around each measured run
  (edgecache_requests_total{outcome}, edgecache_origin_requests_total) - never estimated.
  Raw oha JSON, server logs, the URL list, summary.json (rows) and summary.md (the table)
  land in results/<yyyyMMdd-HHmmss>/ (or -OutDir). Exit code 2 if any mode's success < 0.999.

  Works on Windows PowerShell 5.1 and pwsh 7. Requires: go, oha on PATH.

.EXAMPLE
  pwsh -NoProfile -File scripts/cache.ps1
  # cold, warm, no-cache, origin; 64 connections, 30 s each (+3 s warmup)

.EXAMPLE
  pwsh -NoProfile -File scripts/cache.ps1 -Modes warm -Duration 10s -Distribution uniform

.EXAMPLE
  pwsh -NoProfile -File scripts/cache.ps1 -DiskDir "$env:LOCALAPPDATA\edgecache-bench" -Fresh

.EXAMPLE
  pwsh -NoProfile -File scripts/cache.ps1 -Exe bin/edgecache-pgo.exe -Modes warm -OutDir results/pgo/x/warm-pgo-1 -Quiet
#>
[CmdletBinding()]
param(
    [string[]]$Modes = @('cold', 'warm', 'no-cache', 'origin'),
    [int]$Connections = 64,
    [string]$Duration = '30s',
    [string]$Warmup = '3s',
    [ValidateSet('auto', 'zipf', 'uniform')] [string]$Distribution = 'auto',
    [double]$ZipfExponent = 1.0,
    [int]$UrlListSize = 20000,
    # slow-origin knobs (ORIGIN_* env of cmd/slow-origin)
    [int]$Keyspace = 1000,
    [string]$Latency = '50ms',
    [string]$Jitter = '10ms',
    [int]$BodyBytes = 16384,
    [string]$CacheControl = 'max-age=60',
    # edgecache knobs
    [string]$MemBytes = '256MiB',
    [string]$DiskDir = '',
    [switch]$Fresh,
    [hashtable]$Env = @{},
    # binaries / output
    [switch]$NoBuild,
    [string]$Exe = '',
    [string]$OriginExe = '',
    [string]$Tag = '',
    [string]$OutDir = '',
    [switch]$Quiet,
    [int]$Port = 8081,
    [int]$AdminPort = 9101,
    [int]$OriginPort = 8082
)

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

. (Join-Path $PSScriptRoot '_lib.ps1')

# ---- preflight -----------------------------------------------------------------------------

# `-File cache.ps1 -Modes a,b` arrives as the single string "a,b": split it here.
$Modes = @($Modes | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim().ToLowerInvariant() } | Where-Object { $_ })
if ($Modes.Count -eq 0) { throw 'no modes given' }
$known = @('cold', 'warm', 'no-cache', 'origin')
foreach ($m in $Modes) { if ($known -notcontains $m) { throw "unknown mode '$m' (known: $($known -join ', '))" } }

Assert-Tool -Name 'go' -Install 'winget install GoLang.Go'
Assert-Tool -Name 'oha' -Install 'winget install hatoo.oha / cargo install oha'
Stop-StrayServers
foreach ($pt in @($Port, $AdminPort, $OriginPort)) {
    if (Test-PortBusy -P $pt) { throw "port 127.0.0.1:$pt is already in use; stop whatever is listening there and retry" }
}

if ($Exe) { $NoBuild = $true; $Exe = (Resolve-Path $Exe).Path }
if ($OriginExe) { $OriginExe = (Resolve-Path $OriginExe).Path }
if (-not $Exe) {
    if ($NoBuild) {
        $Exe = Join-Path $script:BinDir 'edgecache.exe'
        if (-not (Test-Path $Exe)) { throw "-NoBuild given but $Exe does not exist" }
    } else {
        Write-Host 'building cmd/edgecache ...' -ForegroundColor DarkGray
        $Exe = Build-Edgecache
    }
}
if (-not $OriginExe) {
    $OriginExe = Join-Path $script:BinDir 'slow-origin.exe'
    if (-not $NoBuild -or -not (Test-Path $OriginExe)) {
        Write-Host 'building cmd/slow-origin ...' -ForegroundColor DarkGray
        $OriginExe = Build-Edgecache -Package './cmd/slow-origin' -Name 'slow-origin'
    }
}

if ($OutDir) {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
    $dir = (Resolve-Path $OutDir).Path
} else {
    $dir = New-ResultDir
}

$diskNote = 'disabled'
if ($DiskDir) {
    if ($Fresh -and (Test-Path $DiskDir)) {
        Write-Host "wiping $DiskDir (-Fresh) ..." -ForegroundColor DarkGray
        Remove-Item -Recurse -Force $DiskDir
    }
    New-Item -ItemType Directory -Path $DiskDir -Force | Out-Null
    $diskNote = "``$DiskDir``"
    if ($Fresh) { $diskNote += ' (wiped)' } else { $diskNote += ' (recovered: a cold row is only cold if the dir was empty)' }
}

# URL distribution: prefer the Zipf list, fall back to the uniform regex.
$proxyBase = "http://127.0.0.1:$Port"
$originBase = "http://127.0.0.1:$OriginPort"
$hasUrlsFile = Test-OhaUrlsFromFile
$useZipf = $false
if ($Distribution -eq 'zipf') {
    if (-not $hasUrlsFile) { throw 'this oha has no --urls-from-file; use -Distribution uniform or upgrade oha' }
    $useZipf = $true
} elseif ($Distribution -eq 'auto') {
    $useZipf = $hasUrlsFile
}
$ohaVersion = ((& oha --version) -join ' ').Trim()
if ($useZipf) {
    $proxyList = Join-Path $dir 'urls-proxy.txt'
    $originList = Join-Path $dir 'urls-origin.txt'
    $distinct = New-ZipfUrlList -Base $proxyBase -Path $proxyList -Keyspace $Keyspace -Count $UrlListSize -Exponent $ZipfExponent
    $null = New-ZipfUrlList -Base $originBase -Path $originList -Keyspace $Keyspace -Count $UrlListSize -Exponent $ZipfExponent
    $distNote = "zipf(s=$ZipfExponent) over $Keyspace keys via ``--urls-from-file`` ($UrlListSize URLs, $distinct distinct; oha $ohaVersion)"
} else {
    $proxyRegex = Get-UniformRegexUrl -Base $proxyBase -Keyspace $Keyspace
    $originRegex = Get-UniformRegexUrl -Base $originBase -Keyspace $Keyspace
    $exact = ''
    if (($Keyspace -ne 1) -and ([math]::Log10($Keyspace) % 1 -ne 0)) { $exact = " (covers 0..10^$(([string]($Keyspace - 1)).Length)-1, wider than the keyspace)" }
    $distNote = "uniform via ``--rand-regex-url '$proxyRegex'``$exact (oha $ohaVersion has no --urls-from-file)"
}

# Invoke-Load runs oha against the proxy or the origin with the chosen distribution.
function Invoke-Load {
    param([string]$Target, [string]$Dur, [string[]]$Headers = @(), [string]$RawOut = '')
    if ($useZipf) {
        $f = $proxyList
        if ($Target -eq 'origin') { $f = $originList }
        return Invoke-Oha -UrlsFile $f -Connections $Connections -Duration $Dur -Headers $Headers -RawOut $RawOut
    }
    $rx = $proxyRegex
    if ($Target -eq 'origin') { $rx = $originRegex }
    return Invoke-Oha -RandRegexUrl $rx -Connections $Connections -Duration $Dur -Headers $Headers -RawOut $RawOut
}

$metricsUrl = "http://127.0.0.1:$AdminPort/metrics"
$rows = @()
$exitCode = 0
$originProc = $null
$cacheProc = $null
$sw = [System.Diagnostics.Stopwatch]::StartNew()

# ---- run -----------------------------------------------------------------------------------

try {
    Write-Host "starting slow-origin on :$OriginPort ($Latency +/- $Jitter, $BodyBytes B, '$CacheControl', keyspace $Keyspace)" -ForegroundColor DarkGray
    $originProc = Start-SlowOrigin -Exe $OriginExe -Port $OriginPort -Latency $Latency -Jitter $Jitter -BodyBytes $BodyBytes -CacheControl $CacheControl -Keyspace $Keyspace -LogFile (Join-Path $dir 'origin.log')

    Write-Host "starting edgecache on :$Port (admin :$AdminPort, mem $MemBytes, disk $diskNote, env: $(Format-EnvLine $Env))" -ForegroundColor DarkGray
    $cacheProc = Start-Edgecache -Exe $Exe -Origin $originBase -Port $Port -AdminPort $AdminPort -MemBytes $MemBytes -DiskDir $DiskDir -Env $Env -LogFile (Join-Path $dir 'edgecache.log')

    $i = 0
    foreach ($mode in $Modes) {
        $i++
        $raw = Join-Path $dir ("{0:00}-{1}.json" -f $i, $mode)
        $target = 'proxy'
        $headers = @()
        $needWarm = $false
        switch ($mode) {
            'cold' { }
            'warm' { $needWarm = $true }
            'no-cache' { $needWarm = $true; $headers = @('Cache-Control: no-cache') }
            'origin' { $target = 'origin' }
        }
        if ($needWarm) {
            Write-Host ("[{0}/{1}] {2}  warmup {3} ..." -f $i, $Modes.Count, $mode, $Warmup) -ForegroundColor Cyan
            $null = Invoke-Load -Target 'proxy' -Dur $Warmup
        }
        Write-Host ("[{0}/{1}] {2}  measuring {3} @ c={4} -> {5} ..." -f $i, $Modes.Count, $mode, $Duration, $Connections, $target) -ForegroundColor Cyan
        $before = Get-Metrics -Url $metricsUrl
        $j = Invoke-Load -Target $target -Dur $Duration -Headers $headers -RawOut $raw
        $after = Get-Metrics -Url $metricsUrl

        $o = ConvertFrom-OhaResult -Oha $j
        $c = Get-CacheStats -Delta (Get-MetricsDelta -Before $before -After $after) -Seconds $o.seconds
        $hit = $c.hit_ratio
        $originRps = $c.origin_rps
        if ($target -eq 'origin') { $hit = [double]::NaN; $originRps = $o.rps }

        $rows += [pscustomobject]@{
            mode        = $mode
            rps         = $o.rps
            p50_ms      = $o.p50_ms
            p90_ms      = $o.p90_ms
            p99_ms      = $o.p99_ms
            hit_ratio   = $hit
            origin_rps  = [math]::Round($originRps, 1)
            success     = $o.success
            requests    = $o.requests
            status      = $o.status
            outcomes    = $c.outcomes
            collapsed   = $c.collapsed
            seconds     = $o.seconds
            connections = $Connections
            duration    = $Duration
            target      = $target
            tag         = $Tag
            raw         = (Split-Path -Leaf $raw)
        }
        if ($o.success -lt 0.999) {
            Write-Warning ("{0}: success {1} < 0.999 (status {2})" -f $mode, (Format-Num $o.success '0.0000'), $o.status)
            $exitCode = 2
        }
    }
} finally {
    if ($cacheProc) { Stop-Server -Process $cacheProc }
    if ($originProc) { Stop-Server -Process $originProc }
}

# ---- write + print -------------------------------------------------------------------------

$lines = @()
$lines += '# cache'
$lines += ''
$lines += (Get-MachineHeader)
$lines += ''
$lines += "- tag: $(if ($Tag) { $Tag } else { '(none)' })"
$lines += "- edgecache: ``$Exe`` (mem $MemBytes, disk $diskNote, env: $(Format-EnvLine $Env))"
$lines += "- slow-origin: ``$OriginExe`` ($Latency +/- $Jitter, $BodyBytes B bodies, ``Cache-Control: $CacheControl``, keyspace $Keyspace)"
$lines += "- oha: -c $Connections -z $Duration per mode (warmup $Warmup, discarded, for warm/no-cache)"
$lines += "- distribution: $distNote"
$lines += '- hit ratio = (hit+stale+revalidated)/all from `edgecache_requests_total` deltas; origin rps from `edgecache_origin_requests_total` deltas (the origin row reports oha rps)'
$lines += ''
$lines += $script:CacheTableHeader
foreach ($r in $rows) { $lines += (Format-CacheRow -Row $r) }
$lines += ''
$lines += '| mode | hit | miss | stale | revalidated | bypass | error | collapsed |'
$lines += '|---|---:|---:|---:|---:|---:|---:|---:|'
foreach ($r in $rows) {
    $oc = $r.outcomes
    $lines += ('| {0} | {1} | {2} | {3} | {4} | {5} | {6} | {7} |' -f $r.mode, $oc['hit'], $oc['miss'], $oc['stale'], $oc['revalidated'], $oc['bypass'], $oc['error'], $r.collapsed)
}
$bad = @($rows | Where-Object { $_.success -lt 0.999 })
if ($bad.Count -gt 0) {
    $lines += ''
    $lines += '**FAILED**: ' + (($bad | ForEach-Object { "$($_.mode) [$($_.status)]" }) -join '; ')
}
$lines += ''
$lines += "_total wall time: $([math]::Round($sw.Elapsed.TotalMinutes, 1)) min_"

$lines | Set-Content -Path (Join-Path $dir 'summary.md') -Encoding utf8
ConvertTo-Json -InputObject @($rows) -Depth 4 | Set-Content -Path (Join-Path $dir 'summary.json') -Encoding utf8

if (-not $Quiet) {
    Write-Host ''
    $lines | ForEach-Object { Write-Host $_ }
    Write-Host ''
}
Write-Host "results: $dir" -ForegroundColor Green
exit $exitCode
