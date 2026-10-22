<#
  构建 mrorsync.exe（Go 版）。
  用法:
    .\build.ps1          只编译
    .\build.ps1 -Test    编译并跑 go test

  Go 工具链查找顺序: $env:GO_BIN -> PATH 里的 go -> %LOCALAPPDATA%\go-toolchain\go\bin\go.exe
#>
param([switch]$Test)

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

Push-Location $PSScriptRoot
try {
    & $go build -trimpath -ldflags '-s -w' -o mrorsync.exe .
    if ($LASTEXITCODE -ne 0) { throw "go build 失败 (exit $LASTEXITCODE)" }
    $exe = Join-Path $PSScriptRoot 'mrorsync.exe'
    $size = [math]::Round((Get-Item $exe).Length / 1MB, 1)
    Write-Host "已生成 $exe ($size MB)"

    if ($Test) {
        & $go test ./...
        if ($LASTEXITCODE -ne 0) { throw "go test 失败 (exit $LASTEXITCODE)" }
    }
}
finally {
    Pop-Location
}
