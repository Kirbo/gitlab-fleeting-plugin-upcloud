# Installs (or upgrades) fleeting-plugin-upcloud on Windows from GitLab releases.
# Run from an elevated PowerShell (Run as administrator):
#
#   irm https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.ps1 | iex
#
# Environment:
#   $env:VERSION              release tag to install, e.g. v1.4.0 (default: latest)
#   $env:INSTALL_PATH         where to install (default: C:\Program Files\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe)
#   $env:SKIP_CHECKSUM_VERIFY "1" installs even if the release publishes no checksum
#
# Upgrading while GitLab Runner is running is safe: Windows can't overwrite a
# running .exe but can rename it, so the old binary is moved aside and the
# runner keeps using it until it restarts.

# Write-Host is deliberate: this is an interactive installer, and its status
# messages shouldn't become pipeline output.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingWriteHost', '')]
param()

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar on Windows PowerShell 5.1

# Windows PowerShell 5.1 may not offer TLS 1.2 by default; gitlab.com requires it.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'kirbo/gitlab-fleeting-plugin-upcloud'
$BinaryName = 'fleeting-plugin-upcloud'
$InstallPath = if ($env:INSTALL_PATH) { $env:INSTALL_PATH } else { Join-Path $env:ProgramFiles "$BinaryName\$BinaryName.exe" }
$InstallDir = Split-Path -Parent $InstallPath
$Version = $env:VERSION

function Fail([string] $Message) {
    Write-Host "Error: $Message" -ForegroundColor Red
    # `exit` would close the console when the script is piped into iex.
    throw $Message
}

# Retries rate limiting (429), server errors (5xx) and network failures, like
# curl --retry in install-plugin.sh; other errors (e.g. a 404 for a mistyped
# VERSION) fail immediately.
function Invoke-WithRetry([scriptblock] $Request) {
    for ($attempt = 1; ; $attempt++) {
        try {
            return & $Request
        } catch {
            $status = 0
            if ($_.Exception.Response) { $status = [int]$_.Exception.Response.StatusCode }
            $retryable = $status -eq 0 -or $status -eq 429 -or $status -ge 500
            if (-not $retryable -or $attempt -ge 5) { throw }
            Write-Host "Request failed ($(if ($status) { "HTTP $status" } else { $_.Exception.Message })); retrying in 5s..."
            Start-Sleep -Seconds 5
        }
    }
}

function Get-InstalledVersion([string] $Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    try {
        $line = & $Path version 2>$null | Select-String -Pattern '^Version:\s+(\S+)'
        if ($line) { return $line.Matches[0].Groups[1].Value }
    } catch {
        Write-Verbose "could not read the version of ${Path}: $_"
    }
    return $null
}

# --- Detect architecture (PROCESSOR_ARCHITEW6432 is set in 32-bit shells on 64-bit Windows) ---
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
    'AMD64' { $Arch = 'amd64' }
    'ARM64' { $Arch = 'arm64' }
    default { Fail "Unsupported architecture: $cpu" }
}

# --- Writing under Program Files needs an elevated shell ---
if ($env:ProgramFiles -and $InstallPath.StartsWith($env:ProgramFiles, [StringComparison]::OrdinalIgnoreCase)) {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        Fail "installing to $InstallDir requires an elevated PowerShell (Run as administrator), or set `$env:INSTALL_PATH."
    }
}

$AssetName = "$BinaryName-windows-$Arch.exe"
$EncodedRepo = [Uri]::EscapeDataString($Repo)
$ApiUrl = if ($Version) {
    "https://gitlab.com/api/v4/projects/$EncodedRepo/releases/$Version"
} else {
    "https://gitlab.com/api/v4/projects/$EncodedRepo/releases/permalink/latest"
}

Write-Host "Detected platform: windows/$Arch"
Write-Host "Fetching $(if ($Version) { $Version } else { 'latest' }) release info from GitLab..."
$release = Invoke-WithRetry { Invoke-RestMethod -Uri $ApiUrl -UseBasicParsing }
$tag = $release.tag_name

function Get-AssetUrl([string] $Name) {
    $link = $release.assets.links | Where-Object { $_.name -eq $Name } | Select-Object -First 1
    if (-not $link) { return $null }
    if ($link.direct_asset_url) { return $link.direct_asset_url }
    return $link.url
}

$downloadUrl = Get-AssetUrl $AssetName
$checksumUrl = Get-AssetUrl "$AssetName.sha256"
if (-not $downloadUrl) {
    $available = ($release.assets.links | ForEach-Object { $_.name }) -join ', '
    Fail "could not find asset '$AssetName' in release $tag. Available assets: $available"
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
try {
    Write-Host "Downloading $AssetName ($tag)..."
    Invoke-WithRetry { Invoke-WebRequest -Uri $downloadUrl -OutFile $tmp -UseBasicParsing }

    # --- Verify checksum (releases publish <asset>.sha256 alongside each binary) ---
    if ($checksumUrl) {
        Write-Host 'Verifying checksum...'
        $checksumFile = (Invoke-WithRetry { Invoke-WebRequest -Uri $checksumUrl -UseBasicParsing }).Content
        if ($checksumFile -is [byte[]]) { $checksumFile = [Text.Encoding]::ASCII.GetString($checksumFile) }
        $expected = ($checksumFile.Trim() -split '\s+')[0].ToLowerInvariant()
        $actual = (Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
        if (-not $expected -or $expected -ne $actual) {
            Fail "checksum mismatch for $AssetName (expected $expected, got $actual)."
        }
        Write-Host 'Checksum OK.'
    } elseif ($env:SKIP_CHECKSUM_VERIFY -eq '1') {
        Write-Warning "no checksum published for $AssetName ($tag); installing unverified (SKIP_CHECKSUM_VERIFY=1)."
    } else {
        Fail "no checksum published for $AssetName ($tag). Set `$env:SKIP_CHECKSUM_VERIFY = '1' to install anyway."
    }

    $previous = Get-InstalledVersion $InstallPath

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    # A running .exe can't be overwritten, but it can be renamed: move the old
    # binary aside, put the new one in place, and clean up old copies that are
    # no longer running (the current one is removed by a later upgrade).
    if (Test-Path -LiteralPath $InstallPath) {
        $stamp = Get-Date -Format 'yyyyMMddHHmmss'
        Move-Item -LiteralPath $InstallPath -Destination "$InstallPath.old-$stamp" -Force
    }
    Move-Item -LiteralPath $tmp -Destination $InstallPath -Force
    Get-ChildItem -Path "$InstallPath.old-*" -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
} finally {
    if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
}

$installed = Get-InstalledVersion $InstallPath
if (-not $installed) { $installed = $tag }

if ($previous) {
    Write-Host "Upgraded: $InstallPath ($previous -> $installed)"
    Write-Host ''
    Write-Host 'GitLab Runner keeps using the old plugin until it restarts. Restart the'
    Write-Host 'service to load the new version (see docs/install-windows.md).'
} else {
    Write-Host "Installed: $InstallPath ($installed)"
    Write-Host ''
    Write-Host 'Reference it in config.toml by its full path (the runner service does not'
    Write-Host 'see PATH changes until a reboot):'
    Write-Host '  [runners.autoscaler]'
    Write-Host "    plugin = '$InstallPath'"
}
