$ErrorActionPreference = "Stop"

$Version = if ($args.Length -ge 1) { $args[0] } else { "" }
$Upload = $args -contains "--upload"
$Repo = $env:REPO

if ([string]::IsNullOrWhiteSpace($Version)) {
    Write-Host "用法: .\scripts\release-local.ps1 <tag> [--upload]"
    Write-Host "示例: .\scripts\release-local.ps1 v1.0.1"
    Write-Host "示例: .\scripts\release-local.ps1 v1.0.1 --upload"
    exit 1
}

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$ReleaseDir = Join-Path $RootDir "release"
New-Item -ItemType Directory -Force -Path $ReleaseDir | Out-Null

if ([string]::IsNullOrWhiteSpace($Repo)) {
    $OriginUrl = ""
    try {
        $OriginUrl = (git -C $RootDir remote get-url origin).Trim()
    } catch {
        $OriginUrl = ""
    }
    if ($OriginUrl -match 'github\.com[:/](.+?)(?:\.git)?$') {
        $Repo = $Matches[1]
    }
}

if ([string]::IsNullOrWhiteSpace($Repo)) {
    Write-Host "无法从 origin 自动识别 GitHub 仓库，请手动设置 REPO=owner/name 后重试。"
    exit 1
}

if (-not (Get-Command wails -ErrorAction SilentlyContinue)) {
    Write-Host "未检测到 wails，请先安装："
    Write-Host "go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0"
    exit 1
}

$Goos = (go env GOOS).Trim()
$Goarch = (go env GOARCH).Trim()

if ($Goos -ne "windows" -or $Goarch -ne "amd64") {
    Write-Host "请在 Windows amd64 环境执行此脚本。当前平台: $Goos/$Goarch"
    exit 1
}

$AssetName = "cdn-iptester-windows-amd64.zip"
$ExePath = $null

Write-Host "开始构建 Windows 桌面包..."
wails build -clean -platform windows/amd64

$ExePath = Get-ChildItem (Join-Path $RootDir "build/bin") -Filter *.exe -File | Select-Object -First 1
if (-not $ExePath) {
    Write-Host "未找到 Windows 可执行文件(.exe)，请检查 Wails 构建输出。"
    exit 1
}

$AssetPath = Join-Path $ReleaseDir $AssetName
if (Test-Path $AssetPath) {
    Remove-Item $AssetPath -Force
}
Compress-Archive -Path $ExePath.FullName -DestinationPath $AssetPath -Force

Write-Host "构建完成: $AssetPath"

if (-not $Upload) {
    Write-Host "如需上传到 GitHub Release，请追加 --upload"
    exit 0
}

if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    Write-Host "未检测到 gh，请先安装 GitHub CLI 后重试上传。"
    exit 1
}

Write-Host "上传目标仓库: $Repo"

gh release view $Version --repo $Repo *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Release $Version 不存在，正在创建..."
    gh release create $Version $AssetPath --repo $Repo --title "CDN-IPtester $Version" --generate-notes
} else {
    Write-Host "Release $Version 已存在，正在上传/覆盖资产..."
    gh release upload $Version $AssetPath --repo $Repo --clobber
}

Write-Host "上传完成: $Version -> $AssetName"
