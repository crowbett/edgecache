# Shared helpers for the edgecache benchmark scripts (perf-lab shape). Dot-source it:
#   . (Join-Path $PSScriptRoot '_lib.ps1')
# Works on Windows PowerShell 5.1 and pwsh 7. No module dependencies.
#
# Contract: cache.ps1 / pgo.ps1 / bench.ps1 only call what is defined here plus their own
# local helpers; anything two scripts need lives in this file.

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'

$script:RepoRoot = Split-Path -Parent $PSScriptRoot
$script:BinDir = Join-Path $script:RepoRoot 'bin'
$script:Inv = [System.Globalization.CultureInfo]::InvariantCulture

# Outcomes edgecache_requests_total{outcome} counts as "served from cache" for the hit ratio.
$script:ServedOutcomes = @('hit', 'stale', 'revalidated')

# ---- build ---------------------------------------------------------------------------------

# Build-Edgecache compiles a cmd/<Package> to bin/<Name>.exe (CGO_ENABLED=0, -trimpath) and
# returns the path. -Pgo: profile path for -pgo=<path>, or 'off'; '' leaves the default
# (-pgo=auto picks up cmd/edgecache/default.pgo when present). -Ldflags '' keeps symbols.
function Build-Edgecache {
    param(
        [string]$Package = './cmd/edgecache',
        [string]$Name = 'edgecache',
        [string]$Pgo = '',
        [string]$Ldflags = '-s -w'
    )
    if (-not (Test-Path $script:BinDir)) { New-Item -ItemType Directory -Path $script:BinDir | Out-Null }
    $out = Join-Path $script:BinDir "$Name.exe"
    $version = Get-GitVersion
    $ld = "$Ldflags -X main.version=$version".Trim()
    $goArgs = @('build', '-trimpath', "-ldflags=$ld", '-o', $out)
    if ($Pgo) { $goArgs += "-pgo=$Pgo" }
    $goArgs += $Package
    Push-Location $script:RepoRoot
    $prevCgo = $env:CGO_ENABLED
    try {
        $env:CGO_ENABLED = '0'
        & go @goArgs
        if ($LASTEXITCODE -ne 0) { throw "go build $Package failed ($LASTEXITCODE)" }
    } finally {
        Pop-Location
        if ($null -eq $prevCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $prevCgo }
    }
    return $out
}

# Get-GitVersion mirrors the Taskfile's VERSION (git describe, else 'dev').
function Get-GitVersion {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $v = & git -C $script:RepoRoot describe --tags --always --dirty 2>$null
        if ($LASTEXITCODE -eq 0 -and $v) { return ([string]$v).Trim() }
        return 'dev'
    } catch { return 'dev' } finally { $ErrorActionPreference = $prev }
}

# Get-PgoBuildSetting returns the -pgo value recorded in a binary's build info ('' if none).
function Get-PgoBuildSetting {
    param([Parameter(Mandatory)] [string]$Exe)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $info = & go version -m $Exe 2>&1 | ForEach-Object { "$_" }
    } finally { $ErrorActionPreference = $prev }
    $line = @($info | Where-Object { $_ -match '^\s*build\s+-pgo=' })
    if ($line.Count -eq 0) { return '' }
    return ($line[0] -replace '^\s*build\s+-pgo=', '').Trim()
}

# ---- processes -----------------------------------------------------------------------------

# Start-Server launches a binary hidden (no console window) with extra env vars, drains its
# stdout/stderr into -LogFile, and waits until -HealthUrl answers 200. Returns the Process.
# Start-Process cannot set a per-process environment on PS 5.1, hence ProcessStartInfo.
function Start-Server {
    param(
        [Parameter(Mandatory)] [string]$Exe,
        [Parameter(Mandatory)] [string]$HealthUrl,
        [hashtable]$Env = @{},
        [string]$LogFile = '',
        [int]$TimeoutSec = 20
    )
    if (-not (Test-Path $Exe)) { throw "binary not found: $Exe" }
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.WorkingDirectory = $script:RepoRoot
    foreach ($k in $Env.Keys) { $psi.EnvironmentVariables[$k] = [string]$Env[$k] }
    $p = New-Object System.Diagnostics.Process
    $p.StartInfo = $psi
    # Subscribe BEFORE starting the async readers or the first lines are lost.
    if ($LogFile) {
        $null = Register-ObjectEvent -InputObject $p -EventName OutputDataReceived -MessageData $LogFile -Action { if ($EventArgs.Data) { Add-Content -Path $Event.MessageData -Value $EventArgs.Data } }
        $null = Register-ObjectEvent -InputObject $p -EventName ErrorDataReceived -MessageData $LogFile -Action { if ($EventArgs.Data) { Add-Content -Path $Event.MessageData -Value $EventArgs.Data } }
    }
    $null = $p.Start()
    $p.BeginOutputReadLine(); $p.BeginErrorReadLine()
    try {
        Wait-Healthz -Url $HealthUrl -Process $p -TimeoutSec $TimeoutSec
    } catch {
        Stop-Server -Process $p
        $tail = ''
        if ($LogFile -and (Test-Path $LogFile)) { $tail = (Get-Content $LogFile -Tail 5) -join "`n" }
        throw "$($_.Exception.Message) (exe $Exe)`n$tail"
    }
    return $p
}

# Start-SlowOrigin starts bin/slow-origin.exe with the ORIGIN_* env and waits for /obj/0.
function Start-SlowOrigin {
    param(
        [Parameter(Mandatory)] [string]$Exe,
        [int]$Port = 8082,
        [string]$Latency = '50ms',
        [string]$Jitter = '10ms',
        [int]$BodyBytes = 16384,
        [string]$CacheControl = 'max-age=60',
        [int]$Keyspace = 1000,
        [string]$LogFile = ''
    )
    $vars = @{
        ORIGIN_ADDR          = "127.0.0.1:$Port"
        ORIGIN_LATENCY       = $Latency
        ORIGIN_JITTER        = $Jitter
        ORIGIN_BODY_BYTES    = "$BodyBytes"
        ORIGIN_CACHE_CONTROL = $CacheControl
        ORIGIN_KEYSPACE      = "$Keyspace"
    }
    return Start-Server -Exe $Exe -HealthUrl "http://127.0.0.1:$Port/obj/0" -Env $vars -LogFile $LogFile
}

# Start-Edgecache starts bin/edgecache.exe in front of an origin and waits for the admin
# /healthz. Extra -Env entries override the defaults set here.
function Start-Edgecache {
    param(
        [Parameter(Mandatory)] [string]$Exe,
        [Parameter(Mandatory)] [string]$Origin,
        [int]$Port = 8081,
        [int]$AdminPort = 9101,
        [string]$MemBytes = '256MiB',
        [string]$DiskDir = '',
        [hashtable]$Env = @{},
        [string]$LogFile = ''
    )
    $e = @{
        EDGECACHE_ORIGIN     = $Origin
        EDGECACHE_ADDR       = "127.0.0.1:$Port"
        EDGECACHE_ADMIN_ADDR = "127.0.0.1:$AdminPort"
        EDGECACHE_MEM_BYTES  = $MemBytes
        LOG_LEVEL            = 'warn'
        LOG_FORMAT           = 'text'
        OTEL_ENABLED         = 'false'
    }
    if ($DiskDir) { $e['EDGECACHE_DISK_DIR'] = $DiskDir }
    foreach ($k in $Env.Keys) { $e[$k] = [string]$Env[$k] }
    return Start-Server -Exe $Exe -HealthUrl "http://127.0.0.1:$AdminPort/healthz" -Env $e -LogFile $LogFile
}

# Wait-Healthz polls until the URL returns 200 or the process dies / timeout hits.
function Wait-Healthz {
    param([Parameter(Mandatory)] [string]$Url, [System.Diagnostics.Process]$Process, [int]$TimeoutSec = 20)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if ($Process -and $Process.HasExited) { throw "server exited early (code $($Process.ExitCode))" }
        try {
            $r = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 2
            if ($r.StatusCode -eq 200) { return }
        } catch { Start-Sleep -Milliseconds 200 }
    }
    throw "timeout after ${TimeoutSec}s waiting for $Url"
}

# Stop-Server terminates a server process (Ctrl+C is not deliverable cross-process on Windows).
function Stop-Server {
    param([System.Diagnostics.Process]$Process)
    if ($Process -and -not $Process.HasExited) { $Process.Kill(); $Process.WaitForExit(5000) | Out-Null }
}

# Stop-StrayServers kills leftover edgecache*/slow-origin* processes started from this
# repo's bin/ by a previous crashed run; anything else with those names is left alone.
function Stop-StrayServers {
    $names = @('edgecache', 'edgecache-prof', 'edgecache-pgo', 'edgecache-nopgo', 'slow-origin')
    $stray = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { ($names -contains $_.ProcessName) -and $_.Path -and $_.Path.StartsWith($script:BinDir, [StringComparison]::OrdinalIgnoreCase) })
    if ($stray.Count -gt 0) {
        Write-Warning ("killing {0} stray process(es) from a previous run: {1}" -f $stray.Count, (($stray | ForEach-Object { "$($_.ProcessName)[$($_.Id)]" }) -join ', '))
        $stray | Stop-Process -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 500
    }
}

# Test-PortBusy reports whether something accepts on 127.0.0.1:<P>.
function Test-PortBusy {
    param([Parameter(Mandatory)] [int]$P)
    $c = New-Object System.Net.Sockets.TcpClient
    try {
        $ar = $c.BeginConnect('127.0.0.1', $P, $null, $null)
        if (-not $ar.AsyncWaitHandle.WaitOne(300)) { return $false }
        $c.EndConnect($ar)
        return $true
    } catch { return $false } finally { $c.Close() }
}

# Assert-Tool throws with the install command when a tool is missing from PATH.
function Assert-Tool {
    param([Parameter(Mandatory)] [string]$Name, [string]$Install = '')
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        $hint = ''
        if ($Install) { $hint = " Install: $Install" }
        throw "$Name is not on PATH.$hint"
    }
}

# ---- oha -----------------------------------------------------------------------------------

# Test-OhaUrlsFromFile reports whether the installed oha accepts --urls-from-file (1.5+).
function Test-OhaUrlsFromFile {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $help = (& oha --help 2>&1 | ForEach-Object { "$_" }) -join "`n" } finally { $ErrorActionPreference = $prev }
    return ($help -match '--urls-from-file')
}

# Invoke-Oha runs oha and returns the parsed JSON summary (--output-format json).
# Exactly one of -Url, -UrlsFile (--urls-from-file, one URL per line, picked at random) or
# -RandRegexUrl (--rand-regex-url) selects the target. -Headers are extra "-H k: v" pairs.
# Key fields: .summary.requestsPerSec, .summary.successRate, .summary.total (s),
# .latencyPercentiles.p50/p90/p99 (s), .statusCodeDistribution.<code>.
function Invoke-Oha {
    param(
        [string]$Url = '',
        [string]$UrlsFile = '',
        [string]$RandRegexUrl = '',
        [int]$Connections = 64,
        [string]$Duration = '15s',
        [string[]]$Headers = @(),
        [string]$RawOut = ''
    )
    $targets = @(@($Url, $UrlsFile, $RandRegexUrl) | Where-Object { $_ }).Count
    if ($targets -ne 1) { throw 'Invoke-Oha needs exactly one of -Url, -UrlsFile, -RandRegexUrl' }
    $ohaArgs = @('-z', $Duration, '-c', "$Connections", '--no-tui', '--output-format', 'json')
    foreach ($h in $Headers) { $ohaArgs += @('-H', $h) }
    if ($UrlsFile) { $ohaArgs += @('--urls-from-file', $UrlsFile) }
    elseif ($RandRegexUrl) { $ohaArgs += @('--rand-regex-url', $RandRegexUrl) }
    else { $ohaArgs += $Url }
    # No 2> redirection: on PowerShell 5.1 a native command's first stderr line under
    # ErrorActionPreference=Stop becomes a terminating NativeCommandError before the exit
    # code is ever checked. Let stderr flow to the console and check the code.
    $json = & oha @ohaArgs
    if ($LASTEXITCODE -ne 0) { throw "oha failed ($LASTEXITCODE)" }
    if ($RawOut) { $json | Set-Content -Path $RawOut -Encoding utf8 }
    return (($json -join "`n") | ConvertFrom-Json)
}

# ConvertFrom-OhaResult reduces an oha JSON object to the row fields every table uses.
# success = oha successRate x share of 2xx responses (a 5xx storm must read as a failure).
function ConvertFrom-OhaResult {
    param([Parameter(Mandatory)] $Oha)
    $total = [long]0; $ok = [long]0; $codes = @()
    foreach ($prop in $Oha.statusCodeDistribution.PSObject.Properties) {
        $n = [long]$prop.Value
        $total += $n
        if ([int]$prop.Name -ge 200 -and [int]$prop.Name -lt 300) { $ok += $n }
        $codes += "$($prop.Name):$n"
    }
    $share2xx = 0.0
    if ($total -gt 0) { $share2xx = $ok / $total }
    return [pscustomobject]@{
        rps      = [math]::Round([double]$Oha.summary.requestsPerSec, 1)
        p50_ms   = [math]::Round([double]$Oha.latencyPercentiles.p50 * 1000, 3)
        p90_ms   = [math]::Round([double]$Oha.latencyPercentiles.p90 * 1000, 3)
        p99_ms   = [math]::Round([double]$Oha.latencyPercentiles.p99 * 1000, 3)
        success  = [math]::Round(([double]$Oha.summary.successRate * $share2xx), 5)
        seconds  = [double]$Oha.summary.total
        requests = $total
        status   = ($codes -join ' ')
    }
}

# New-ZipfUrlList writes -Count URLs "<Base>/obj/<n>" (n in 0..Keyspace-1) drawn from a
# Zipf(s=Exponent) distribution to -Path (one per line, for --urls-from-file) and returns
# the number of distinct keys drawn. Seeded, so two runs see the same list.
function New-ZipfUrlList {
    param(
        [Parameter(Mandatory)] [string]$Base,
        [Parameter(Mandatory)] [string]$Path,
        [int]$Keyspace = 1000,
        [int]$Count = 20000,
        [double]$Exponent = 1.0,
        [int]$Seed = 42
    )
    if ($Keyspace -lt 1) { throw 'Keyspace must be >= 1' }
    # Cumulative weights 1/(k+1)^s over k = 0..Keyspace-1, then inverse-CDF sampling.
    $cdf = New-Object 'double[]' $Keyspace
    $sum = 0.0
    for ($k = 0; $k -lt $Keyspace; $k++) { $sum += [math]::Pow($k + 1, -$Exponent); $cdf[$k] = $sum }
    $rng = New-Object System.Random $Seed
    $seen = @{}
    $sb = New-Object System.Text.StringBuilder
    for ($i = 0; $i -lt $Count; $i++) {
        $u = $rng.NextDouble() * $sum
        $idx = [array]::BinarySearch($cdf, $u)
        if ($idx -lt 0) { $idx = -$idx - 1 }
        if ($idx -ge $Keyspace) { $idx = $Keyspace - 1 }
        $seen[$idx] = $true
        [void]$sb.Append($Base).Append('/obj/').Append($idx).Append("`n")
    }
    [System.IO.File]::WriteAllText($Path, $sb.ToString())
    return $seen.Count
}

# Get-UniformRegexUrl builds the --rand-regex-url pattern for /obj/<n>: zero-padded
# [0-9]{d} (d = digits of Keyspace-1), so every one of the 10^d paths is equally likely
# (an alternation would be branch-uniform, not value-uniform) and exactly 10^d distinct
# URLs exist. Exact when Keyspace is a power of ten; otherwise it covers 0..10^d-1 and the
# caller records that. Dots are written [.]: oha 1.16's generator does not honour \. .
function Get-UniformRegexUrl {
    param([Parameter(Mandatory)] [string]$Base, [int]$Keyspace = 1000)
    $d = ([string]([math]::Max($Keyspace - 1, 0))).Length
    $escaped = $Base -replace '\.', '[.]'
    return "$escaped/obj/[0-9]{$d}"
}

# ---- metrics -------------------------------------------------------------------------------

# ConvertFrom-Exposition parses Prometheus text (0.0.4) into a hashtable
# "name{labels}" -> [double]; comment and blank lines are skipped, +Inf/-Inf/NaN handled.
function ConvertFrom-Exposition {
    param([Parameter(Mandatory)] [AllowEmptyString()] [string]$Text)
    $m = @{}
    foreach ($raw in ($Text -split "`r?`n")) {
        $line = $raw.Trim()
        if (-not $line -or $line.StartsWith('#')) { continue }
        if ($line -notmatch '^([A-Za-z_:][A-Za-z0-9_:]*)(\{[^}]*\})?\s+(\S+)(\s+\S+)?$') { continue }
        $key = $Matches[1] + $Matches[2]
        $v = $Matches[3]
        if ($v -eq '+Inf') { $m[$key] = [double]::PositiveInfinity }
        elseif ($v -eq '-Inf') { $m[$key] = [double]::NegativeInfinity }
        elseif ($v -eq 'NaN') { $m[$key] = [double]::NaN }
        else { $m[$key] = [double]::Parse($v, [System.Globalization.NumberStyles]::Float, $script:Inv) }
    }
    return $m
}

# Get-Metrics scrapes a /metrics URL and returns the parsed hashtable.
function Get-Metrics {
    param([Parameter(Mandatory)] [string]$Url)
    $r = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 5
    if ($r.StatusCode -ne 200) { throw "GET $Url -> $($r.StatusCode)" }
    return ConvertFrom-Exposition -Text ([string]$r.Content)
}

# Get-MetricsDelta returns after - before for every series in -After (missing in -Before
# counts as 0, so a series that first appears mid-run still delta's correctly).
function Get-MetricsDelta {
    param([Parameter(Mandatory)] [hashtable]$Before, [Parameter(Mandatory)] [hashtable]$After)
    $d = @{}
    foreach ($k in $After.Keys) {
        $b = 0.0
        if ($Before.ContainsKey($k)) { $b = [double]$Before[$k] }
        $d[$k] = [double]$After[$k] - $b
    }
    return $d
}

# Get-MetricSum adds every series of -Name whose labels match all -Labels (hashtable);
# an empty -Labels sums the whole family. Returns 0 when nothing matches.
function Get-MetricSum {
    param([Parameter(Mandatory)] [hashtable]$Metrics, [Parameter(Mandatory)] [string]$Name, [hashtable]$Labels = @{})
    $sum = 0.0
    foreach ($k in $Metrics.Keys) {
        if ($k -ne $Name -and -not $k.StartsWith("$Name{")) { continue }
        $ok = $true
        foreach ($lk in $Labels.Keys) {
            if ($k -notmatch ('[{,]' + [regex]::Escape($lk) + '="' + [regex]::Escape([string]$Labels[$lk]) + '"[,}]')) { $ok = $false; break }
        }
        if ($ok) { $sum += [double]$Metrics[$k] }
    }
    return $sum
}

# Get-CacheStats reduces a metrics delta to the numbers the tables print: requests by
# outcome, hit ratio (hit+stale+revalidated over everything), origin requests.
# hit_ratio is NaN when no request was counted (a direct-to-origin control, say).
function Get-CacheStats {
    param([Parameter(Mandatory)] [hashtable]$Delta, [double]$Seconds = 0)
    $total = Get-MetricSum -Metrics $Delta -Name 'edgecache_requests_total'
    $served = 0.0
    foreach ($o in $script:ServedOutcomes) { $served += Get-MetricSum -Metrics $Delta -Name 'edgecache_requests_total' -Labels @{ outcome = $o } }
    $origin = Get-MetricSum -Metrics $Delta -Name 'edgecache_origin_requests_total'
    $ratio = [double]::NaN
    if ($total -gt 0) { $ratio = $served / $total }
    $originRps = [double]::NaN
    if ($Seconds -gt 0) { $originRps = $origin / $Seconds }
    $byOutcome = @{}
    foreach ($o in @('hit', 'miss', 'stale', 'revalidated', 'bypass', 'error')) {
        $byOutcome[$o] = [long](Get-MetricSum -Metrics $Delta -Name 'edgecache_requests_total' -Labels @{ outcome = $o })
    }
    return [pscustomobject]@{
        requests   = [long]$total
        served     = [long]$served
        hit_ratio  = $ratio
        origin_req = [long]$origin
        origin_rps = $originRps
        outcomes   = $byOutcome
        collapsed  = [long](Get-MetricSum -Metrics $Delta -Name 'edgecache_collapsed_total')
    }
}

# ---- results -------------------------------------------------------------------------------

# New-ResultDir creates results/<yyyyMMdd-HHmmss>/ (or results/<Experiment>/<ts>/) and returns it.
function New-ResultDir {
    param([string]$Experiment = '')
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $dir = Join-Path $script:RepoRoot 'results'
    if ($Experiment) { $dir = Join-Path $dir $Experiment }
    $dir = Join-Path $dir $stamp
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    return $dir
}

# Get-MachineHeader returns a markdown line describing the box, for result files.
function Get-MachineHeader {
    $cpu = $env:PROCESSOR_IDENTIFIER
    try {
        $proc = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop | Select-Object -First 1
        if ($proc -and $proc.Name) { $cpu = $proc.Name.Trim() }
    } catch { }
    $goVer = (& go version) -replace '^go version ', ''
    return "_$(Get-Date -Format 'yyyy-MM-dd HH:mm') | $cpu | $goVer | $([Environment]::OSVersion.VersionString) | PowerShell $($PSVersionTable.PSVersion)_"
}

# Format-Num renders a number with an invariant culture ('n/a' for NaN).
function Format-Num {
    param([double]$V, [string]$Fmt = '0.00')
    if ([double]::IsNaN($V)) { return 'n/a' }
    return $V.ToString($Fmt, $script:Inv)
}

# Format-Ratio renders a hit ratio as a percentage ('n/a' for NaN).
function Format-Ratio {
    param([double]$V)
    if ([double]::IsNaN($V)) { return 'n/a' }
    return ($V * 100).ToString('0.0', $script:Inv) + '%'
}

# Format-EnvLine renders a hashtable as "k=v, k=v" (sorted) or '(none)'.
function Format-EnvLine {
    param([hashtable]$H)
    if (-not $H -or $H.Count -eq 0) { return '(none)' }
    return (($H.Keys | Sort-Object | ForEach-Object { "$_=$($H[$_])" }) -join ', ')
}

# Format-CacheRow renders one summary.md table row:
# | mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |
function Format-CacheRow {
    param([Parameter(Mandatory)] $Row)
    return ('| {0} | {1} | {2} | {3} | {4} | {5} | {6} | {7} |' -f $Row.mode,
        (Format-Num $Row.rps '0'), (Format-Num $Row.p50_ms), (Format-Num $Row.p90_ms), (Format-Num $Row.p99_ms),
        (Format-Ratio $Row.hit_ratio), (Format-Num $Row.origin_rps '0'), (Format-Num $Row.success '0.0000'))
}

$script:CacheTableHeader = @(
    '| mode | rps | p50 ms | p90 ms | p99 ms | hit ratio | origin rps | success |',
    '|---|---:|---:|---:|---:|---:|---:|---:|'
)
