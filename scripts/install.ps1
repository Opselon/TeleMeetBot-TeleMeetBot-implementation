#requires -Version 5.1
<#
.SYNOPSIS
  TeleMeetBot one-command installer for Windows.

.DESCRIPTION
 irm https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.ps1 | iex

  What it does:
    1. Detects OS, CPU architecture, dependencies and administrator privileges
    2. Downloads the matching release archive from a pinned trusted release
    3. Verifies the SHA-256 checksum published with the release
    4. Installs the telemeetbot.exe binary
    5. Stages a fail-closed template config (never overwrites an existing one)
    6. Optionally registers a Windows service (needs an elevated shell)
    7. Runs the interactive setup wizard (unless -NoWizard), then verifies health

.PARAMETER Version
  The release tag to install, e.g. v1.2.0. Defaults to the latest release.
  The installer refuses anything that does not look like a tag.

.PARAMETER Prefix
  Install root. Default: $env:ProgramFiles\TeleMeetBot.

.PARAMETER ConfigDir
  Config directory. Default: $env:ProgramData\TeleMeetBot.

.PARAMETER DataDir
  Data directory. Default: <ConfigDir>\data.

.PARAMETER NoWizard
  Skip the interactive setup wizard.

.PARAMETER NoService
  Do not register the Windows service.

.PARAMETER NoStart
  Do not start the service after install.

.PARAMETER DryRun
  Verify the download and checksum, then report what would be installed.

.EXAMPLE
  irm https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.ps1 | iex
#>
[CmdletBinding()]
param(
  [string]$Version = "latest",
  [string]$Prefix = "",
  [string]$ConfigDir = "",
  [string]$DataDir = "",
  [switch]$NoWizard,
  [switch]$NoService,
  [switch]$NoStart,
  [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest's progress bar makes downloads 10x slower

$Repo = "Opselon/TeleMeetBot-TeleMeetBot-implementation"

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Warn([string]$msg) { Write-Host "warn: $msg" -ForegroundColor Yellow }
function Die([string]$msg) {
  Write-Host "error: $msg" -ForegroundColor Red
  exit 1
}

# ---- 1. Platform detection ------------------------------------------------
# PowerShell does not give us a clean "is Windows" on old versions; the OS
# check is done by the presence of $env:ProgramFiles (set on every Windows).
if (-not $env:ProgramFiles -and -not $env:ProgramW6432) {
  Die "install.ps1 is for Windows. On Linux/macOS use install.sh, or Docker."
}

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  "x86"   { "386" }
  default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

if (-not $Prefix)    { $Prefix    = Join-Path $env:ProgramFiles "TeleMeetBot" }
if (-not $ConfigDir) { $ConfigDir = Join-Path $env:ProgramData "TeleMeetBot" }
if (-not $DataDir)   { $DataDir   = Join-Path $ConfigDir "data" }

$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
  [Security.Principal.WindowsBuiltInRole]::Administrator)

# ---- 2. Resolve the pinned trusted release --------------------------------
if ($Version -eq "latest") {
  Write-Step "resolving the latest release"
  try {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
      -TimeoutSec 20 -Headers @{ "User-Agent" = "telemeetbot-installer" }
    $Version = $rel.tag_name
  } catch {
    Die "cannot reach the GitHub API: $($_.Exception.Message)"
  }
  if (-not $Version) { Die "could not resolve the latest release" }
}
if ($Version -notmatch '^v\d+\.\d+\.\d+') {
  Die "resolved version '$Version' does not look like a tag (vX.Y.Z); refusing to install"
}

$file = "telemeetbot-windows-$arch.zip"
# $BaseUrl replaces the *whole* download root, so a mirror serves the release
# assets directly. The default is the GitHub release download directory for
# the pinned version; asset names are identical either way, which keeps the
# checksum verification honest.
$base = "${BaseUrl:https://github.com/$Repo/releases/download/$Version}"
$tmp = New-Item -ItemType Directory -Path ([System.IO.Path]::GetTempPath()) -Name "telemeetbot-install-$(Get-Random)" -Force

# ---- 3. Download + checksum verify ----------------------------------------
Write-Step "downloading TeleMeetBot $Version (windows/$arch)"
try {
  Invoke-WebRequest -Uri "$base/$file" -OutFile (Join-Path $tmp $file) -TimeoutSec 120
  Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS") -TimeoutSec 60
} catch {
  Die "download failed: $($_.Exception.Message)"
}

Write-Step "verifying SHA-256 checksum"
$sums = Get-Content -Path (Join-Path $tmp "SHA256SUMS") -ErrorAction SilentlyContinue
if (-not $sums) { Die "SHA256SUMS is missing or unreadable; refusing to install unverified artifacts" }
$expected = $null
foreach ($line in $sums) {
  # Handles both "hash  file" and "hash  ./file" forms.
  if ($line -match "^\s*([0-9a-fA-F]{64})\s+\.*[/\\]?$([regex]::Escape($file))\s*$") {
    $expected = $Matches[1].ToLower()
    break
  }
}
if (-not $expected) { Die "SHA256SUMS has no entry for $file; refusing to install unverified artifacts" }

$actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $file)).Hash.ToLower()
if ($actual -ne $expected) {
  Die "checksum mismatch for $file`n  expected $expected`n  actual   $actual`nThe download is corrupt or was tampered with; aborting."
}

if ($DryRun) {
  Write-Step "dry run: verified $file, would install to $Prefix"
  exit 0
}

# ---- 4. Install the binary ------------------------------------------------
Write-Step "installing binary to $Prefix"
$stage = Join-Path $tmp "extract"
Expand-Archive -Path (Join-Path $tmp $file) -DestinationPath $stage -Force
$exe = Join-Path $stage "telemeetbot.exe"
if (-not (Test-Path $exe)) { Die "archive does not contain telemeetbot.exe" }

if ($isAdmin) {
  if (-not (Test-Path $Prefix)) { New-Item -ItemType Directory -Path $Prefix -Force | Out-Null }
  if (-not (Test-Path $ConfigDir)) { New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null }
  if (-not (Test-Path $DataDir)) { New-Item -ItemType Directory -Path $DataDir -Force | Out-Null }
  Copy-Item -Path $exe -Destination (Join-Path $Prefix "telemeetbot.exe") -Force
} else {
  # Per-user install: fall back to LOCALAPPDATA so an unprivileged user can
  # still install; the service registration is then skipped (needs admin).
  $Prefix = Join-Path $env:LOCALAPPDATA "Programs\TeleMeetBot"
  if (-not (Test-Path $Prefix)) { New-Item -ItemType Directory -Path $Prefix -Force | Out-Null }
  Copy-Item -Path $exe -Destination (Join-Path $Prefix "telemeetbot.exe") -Force
  if (-not (Test-Path $ConfigDir)) { New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null }
  if (-not (Test-Path $DataDir)) { New-Item -ItemType Directory -Path $DataDir -Force | Out-Null }
  if (-not $NoService) {
    Write-Warn "service registration needs an elevated shell; install the service later with an administrator prompt."
    $NoService = $true
  }
}

# ---- 5. Config ------------------------------------------------------------
$configPath = Join-Path $ConfigDir "config.yml"
$example = Join-Path $stage "deploy\config.example.yml"
if (-not (Test-Path $configPath) -and (Test-Path $example)) {
  Copy-Item -Path $example -Destination $configPath -Force
  Write-Step "staged a template config at $configPath - the wizard will edit it"
}

# ---- 6. Windows service ---------------------------------------------------
if (-not $NoService -and $isAdmin) {
  Write-Step "registering the Windows service"
  $binPath = "`"$(Join-Path $Prefix 'telemeetbot.exe')`" --config `"$configPath`""
  & sc.exe stop telemeetbot 2>$null | Out-Null
  & sc.exe delete telemeetbot 2>$null | Out-Null
  & sc.exe create telemeetbot binPath= $binPath DisplayName= "TeleMeetBot" start= auto 2>&1 | ForEach-Object { Write-Host $_ }
  if ($LASTEXITCODE -ne 0) {
    Write-Warn "service registration failed; run the binary manually: $(Join-Path $Prefix 'telemeetbot.exe') --config `"$configPath`""
  } else {
    & sc.exe description telemeetbot "Google Meet streaming bot with a Telegram dashboard" | Out-Null
    if (-not $NoStart) {
      & sc.exe start telemeetbot 2>&1 | ForEach-Object { Write-Host $_ }
    }
  }
}

# ---- Chromium availability check ------------------------------------------
$chr = Get-Command chromium -ErrorAction SilentlyContinue
if (-not $chr) { Write-Warn "no Chromium found - the setup wizard can install one with winget (or use Docker)" }

# ---- 7. Wizard + health verification --------------------------------------
$binFinal = Join-Path $Prefix "telemeetbot.exe"
if (-not $NoWizard) {
  Write-Step "starting the setup wizard (Ctrl+C to abort). Config: $configPath"
  & $binFinal --wizard --config $configPath
  Write-Step "next steps"
  if ($isAdmin -and -not $NoService) {
    Write-Host "    sc.exe start telemeetbot"
    Write-Host "    Get-EventLog -LogName Application -Source telemeetbot -Newest 20"
  } else {
    Write-Host "    $binFinal --config `"$configPath`""
  }
} else {
  Write-Step "installed. Run the wizard: $binFinal --wizard --config `"$configPath`""
}

if (-not $NoStart -and -not $NoService -and $isAdmin) {
  Write-Step "verifying the health endpoint"
  Start-Sleep -Seconds 2
  try {
    $live = Invoke-WebRequest -Uri "http://127.0.0.1:18080/livez" -TimeoutSec 5 -UseBasicParsing
    if ($live.StatusCode -eq 200) {
      Write-Step "health check passed (http://127.0.0.1:18080/livez)"
    }
  } catch {
    Write-Warn "health endpoint not answering yet - check the service (it may still be starting)"
  }
}
