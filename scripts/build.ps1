$ErrorActionPreference = "Stop"

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$DistDir = Join-Path $RootDir "dist"
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $DistDir "configs") | Out-Null
Copy-Item (Join-Path $RootDir "configs/config.example.yaml") (Join-Path $DistDir "configs/config.example.yaml") -Force
if (Test-Path (Join-Path $RootDir "configs/config.yaml")) {
    Copy-Item (Join-Path $RootDir "configs/config.yaml") (Join-Path $DistDir "configs/config.yaml") -Force
}

$Goos = (go env GOOS).Trim()
$Goarch = (go env GOARCH).Trim()
$Ext = ""
if ($Goos -eq "windows") {
    $Ext = ".exe"
}

if (Get-Command wails -ErrorAction SilentlyContinue) {
    Write-Host "building desktop bundle with wails"
    wails build
    exit 0
}

Write-Host "building $Goos/$Goarch"
go build -trimpath -ldflags="-s -w" -o (Join-Path $DistDir "cdn-iptester-$Goos-$Goarch$Ext") (Join-Path $RootDir "cmd/cdn-iptester")
