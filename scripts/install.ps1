# Permafrost (pfrost) - One-Line Universal Installer for Windows (PowerShell)
# Usage: irm https://raw.githubusercontent.com/amglogicalis/permafrost/main/scripts/install.ps1 | iex

param(
    [switch]$Local
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$Repo = "amglogicalis/permafrost"
$InstallDir = "$env:USERPROFILE\.permafrost\bin"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Write-Host "[Permafrost] Installing pfrost..." -ForegroundColor Cyan

$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -match "ARM64") {
    $Arch = "arm64"
}

if ($Local -and (Test-Path ".\pfrost.cmd")) {
    Copy-Item -Force ".\pfrost.cmd" "$InstallDir\pfrost.cmd"
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($UserPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable("Path", "$InstallDir;$UserPath", "User")
    }
    Write-Host "[OK] Installed local pfrost launcher to $InstallDir\pfrost.cmd" -ForegroundColor Green
    exit 0
}

try {
    $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $Tag = $Release.tag_name
    $Version = $Tag.TrimStart("v")
    $ZipName = "pfrost_${Version}_windows_${Arch}.zip"
    $ZipUrl = "https://github.com/$Repo/releases/download/$Tag/$ZipName"
    $ChecksumsUrl = "https://github.com/$Repo/releases/download/$Tag/checksums.txt"

    $TmpDir = Join-Path $env:TEMP ("pfrost-install-" + [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

    $ZipPath = Join-Path $TmpDir $ZipName
    $ChecksumsPath = Join-Path $TmpDir "checksums.txt"

    Write-Host "  * Downloading $Tag (windows/$Arch)..."
    curl.exe -fsSL $ZipUrl -o $ZipPath
    curl.exe -fsSL $ChecksumsUrl -o $ChecksumsPath

    $ActualHash = (Get-FileHash -Path $ZipPath -Algorithm SHA256).Hash.ToLower()
    $ChecksumsContent = Get-Content $ChecksumsPath
    $ExpectedLine = $ChecksumsContent | Where-Object { $_ -match $ZipName }
    if ($ExpectedLine) {
        $ExpectedHash = ($ExpectedLine -split "\s+")[0].ToLower()
        if ($ActualHash -ne $ExpectedHash) {
            throw "SHA-256 checksum mismatch for $ZipName"
        }
        Write-Host "  * SHA-256 checksum verified [OK]" -ForegroundColor Green
    }

    Expand-Archive -Path $ZipPath -DestinationPath $TmpDir -Force
    Copy-Item -Force (Join-Path $TmpDir "pfrost.exe") (Join-Path $InstallDir "pfrost.exe")
    Remove-Item -Recurse -Force $TmpDir
} catch {
    if (Test-Path ".\pfrost.cmd") {
        Copy-Item -Force ".\pfrost.cmd" "$InstallDir\pfrost.cmd"
    } else {
        throw $_
    }
}

$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($UserPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$InstallDir;$UserPath", "User")
    Write-Host "  * Added $InstallDir to User PATH [OK]" -ForegroundColor Green
}

Write-Host ""
Write-Host "[OK] Permafrost (pfrost) installed successfully to $InstallDir" -ForegroundColor Green
Write-Host "     Run 'pfrost' in your terminal to open the interactive TUI." -ForegroundColor Cyan
