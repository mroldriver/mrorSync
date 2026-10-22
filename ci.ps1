<#
  本地/CI 检查：格式、vet、测试，可选构建。
  用法:
    .\ci.ps1              # gofmt + vet + test
    .\ci.ps1 -Build       # 再加一次构建

  Gitee Go 或其它 CI 可以直接执行本脚本，无需重复安装步骤。
#>
param([switch]$Build)

$ErrorActionPreference = 'Stop'

$candidates = @()
if ($env:GO_BIN) { $candidates += $env:GO_BIN }
$cmd = Get-Command go -ErrorAction SilentlyContinue
if ($cmd) { $candidates += $cmd.Source }
$candidates += (Join-Path $env:LOCALAPPDATA 'go-toolchain\go\bin\go.exe')

$go = $candidates | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $go) {
    throw '找不到 go 命令。请安装 Go（https://go.dev/dl/），或设置 GO_BIN 指向 go.exe。'
}
Write-Host "使用 Go: $go"

$gofmt = Join-Path (Split-Path $go) 'gofmt.exe'
if (-not (Test-Path $gofmt)) {
    $cmd = Get-Command gofmt -ErrorAction SilentlyContinue
    if (-not $cmd) { throw '找不到 gofmt' }
    $gofmt = $cmd.Source
}

Push-Location $PSScriptRoot
try {
    $files = Get-ChildItem -Recurse -Filter *.go -File |
        Where-Object { $_.FullName -notmatch '\\\.git\\' } |
        ForEach-Object { $_.FullName }
    $bad = & $gofmt -l @files
    if ($bad) {
        $bad | ForEach-Object { Write-Host "未格式化: $_" }
        throw 'gofmt 未通过'
    }
    Write-Host 'gofmt OK'

    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet 失败' }
    Write-Host 'go vet OK'

    & $go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test 失败' }
    Write-Host 'go test OK'

    if ($Build) {
        & (Join-Path $PSScriptRoot 'build.ps1')
    }
}
finally {
    Pop-Location
}
