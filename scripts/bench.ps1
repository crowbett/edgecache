<#
.SYNOPSIS
Runs the edgecache hot-path microbenchmarks with proper statistics and compares them with benchstat.

.DESCRIPTION
Runs `go test -run '^$' -bench <Run> -benchmem -count <Count> <Pkg>` with CGO_ENABLED=0,
streams the output to the console and to a file, and optionally compares that file against a
committed baseline with benchstat.

Output goes to .bench/<Save>.txt when -Save is given (with a sibling .bench/<Save>.meta.txt
holding CPU / Go version / OS / command), otherwise to results/bench/<timestamp>.txt, which is
gitignored.

Works on Windows PowerShell 5.1 and pwsh 7. Requires: go (benchstat for -Compare).

.PARAMETER Pkg
Package pattern passed to go test. Default './internal/bench/...' (add ./internal/metrics/ for
BenchmarkHandler).

.PARAMETER Run
Benchmark regex passed to -bench. Default '.' (everything).

.PARAMETER Count
Repetitions per benchmark (-count). Default 10; benchstat wants >= 6 for a usable confidence interval.

.PARAMETER Save
Write results to .bench/<Save>.txt (+ .meta.txt). Use 'baseline' only in a dedicated commit.

.PARAMETER Compare
After the run, `benchstat .bench/<Compare>.txt <new>` where <new> is this run's output file.

.PARAMETER Timeout
go test -timeout. Default '1h'.

.EXAMPLE
pwsh -NoProfile -File scripts/bench.ps1 -Run 'HitPath|KeyBuild' -Count 6 -Compare baseline

.EXAMPLE
pwsh -NoProfile -File scripts/bench.ps1 -Save baseline
#>
[CmdletBinding()]
param(
    [string]$Pkg = './internal/bench/...',
    [string]$Run = '.',
    [int]$Count = 10,
    [string]$Save = '',
    [string]$Compare = '',
    [string]$Timeout = '1h'
)

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot '_lib.ps1')

$BenchDir = Join-Path $script:RepoRoot '.bench'
$ResultsDir = Join-Path $script:RepoRoot 'results\bench'

function Get-MachineMeta {
    param([string[]]$GoArgs, [string]$OutFile, [datetime]$Started, [timespan]$Elapsed)
    $cpu = $env:PROCESSOR_IDENTIFIER
    try {
        $proc = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop | Select-Object -First 1
        if ($proc -and $proc.Name) { $cpu = $proc.Name.Trim() }
    } catch { }
    $goVer = (& go version) -replace '^go version ', ''
    return @(
        "date:      $($Started.ToString('yyyy-MM-dd HH:mm:ss zzz'))",
        "elapsed:   $([int]$Elapsed.TotalSeconds)s",
        "cpu:       $cpu",
        "cores:     $([Environment]::ProcessorCount) logical",
        "os:        $([Environment]::OSVersion.VersionString)",
        "go:        $goVer",
        "shell:     PowerShell $($PSVersionTable.PSVersion)",
        "command:   CGO_ENABLED=0 go $($GoArgs -join ' ')",
        "count:     $Count",
        "results:   $OutFile"
    )
}

# --- validate ----------------------------------------------------------------

if ($Count -lt 1) { throw "-Count must be >= 1 (got $Count)" }
if ($Save -and $Compare -and ($Save -eq $Compare)) {
    throw "-Save and -Compare are both '$Save': that would overwrite the baseline and compare it with itself"
}
foreach ($n in @($Save, $Compare)) {
    if ($n -and ($n -notmatch '^[A-Za-z0-9._-]+$')) { throw "name '$n' must match ^[A-Za-z0-9._-]+$" }
}

Assert-Tool -Name 'go' -Install 'winget install GoLang.Go'
if ($Compare) { Assert-Tool -Name 'benchstat' -Install 'go install golang.org/x/perf/cmd/benchstat@latest' }

$baseFile = ''
if ($Compare) {
    $baseFile = Join-Path $BenchDir "$Compare.txt"
    if (-not (Test-Path $baseFile)) { throw "baseline not found: $baseFile (create it with -Save $Compare)" }
}

if ($Save) {
    if (-not (Test-Path $BenchDir)) { New-Item -ItemType Directory -Path $BenchDir | Out-Null }
    $outFile = Join-Path $BenchDir "$Save.txt"
    $metaFile = Join-Path $BenchDir "$Save.meta.txt"
} else {
    if (-not (Test-Path $ResultsDir)) { New-Item -ItemType Directory -Path $ResultsDir -Force | Out-Null }
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $outFile = Join-Path $ResultsDir "$stamp.txt"
    $metaFile = Join-Path $ResultsDir "$stamp.meta.txt"
}

$goArgs = @('test', '-run', '^$', '-bench', $Run, '-benchmem', '-count', "$Count", '-timeout', $Timeout, $Pkg)

# --- run ---------------------------------------------------------------------

Write-Host "==> CGO_ENABLED=0 go $($goArgs -join ' ')"
Write-Host "==> writing $outFile"

# Open the output first: a second run on the same name fails here on the file lock
# before it can touch anything else.
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$writer = New-Object System.IO.StreamWriter($outFile, $false, $utf8NoBom)
$writer.NewLine = "`n"
$writer.AutoFlush = $true # a killed run still leaves every finished line on disk

$started = Get-Date
$prevCgo = $env:CGO_ENABLED
$prevEap = $ErrorActionPreference
$exit = -1
$benchLines = 0
Push-Location $script:RepoRoot
try {
    $env:CGO_ENABLED = '0'
    # 'Continue' so that go's stderr (compile errors, warnings) streams through 2>&1 on
    # Windows PowerShell 5.1 instead of becoming a terminating NativeCommandError.
    $ErrorActionPreference = 'Continue'
    & go @goArgs 2>&1 | ForEach-Object {
        $line = "$_"
        Write-Host $line
        $writer.WriteLine($line)
        if ($line -match '^Benchmark') { $benchLines++ }
    }
    $exit = $LASTEXITCODE
} finally {
    $ErrorActionPreference = $prevEap
    if ($null -eq $prevCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $prevCgo }
    $writer.Dispose()
    Pop-Location
}

if ($exit -ne 0) {
    throw "go test failed with exit code $exit (output kept in $outFile)"
}
if ($benchLines -eq 0) {
    throw "no benchmark matched -Run '$Run' in $Pkg (output kept in $outFile)"
}
# Meta is written only for a successful run, so it always describes the results next to it.
$meta = Get-MachineMeta -GoArgs $goArgs -OutFile $outFile -Started $started -Elapsed ((Get-Date) - $started)
[System.IO.File]::WriteAllLines($metaFile, [string[]]$meta)
Write-Host "==> $benchLines benchmark lines -> $outFile"
Write-Host "==> machine info -> $metaFile"

# --- compare -----------------------------------------------------------------

if ($Compare) {
    # Relative paths keep benchstat's column headers short.
    $baseRel = ".bench/$Compare.txt"
    $outRel = $outFile.Substring($script:RepoRoot.Length).TrimStart('\', '/') -replace '\\', '/'
    # Scope the table to what actually ran, otherwise every baseline row shows up empty.
    $bsArgs = @()
    if ($Run -ne '.') { $bsArgs += @('-filter', ".name:/$Run/") }
    $bsArgs += @($baseRel, $outRel)
    Write-Host ""
    Write-Host "==> benchstat $($bsArgs -join ' ')"
    Push-Location $script:RepoRoot
    try {
        & benchstat @bsArgs
        $bsExit = $LASTEXITCODE
    } finally { Pop-Location }
    if ($bsExit -ne 0) { throw "benchstat failed with exit code $bsExit" }
}
