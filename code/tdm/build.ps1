# 一键编译 Xlauch 下载器：GUI 子系统（不弹控制台）+ 去除调试符号
# 用法：powershell -ExecutionPolicy Bypass -File .\build.ps1
$proj = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $proj
. "$proj\..\..\tools\go-env.ps1"
go build -trimpath -ldflags "-s -w -H=windowsgui" -o "..\..\dist\Xlauch 下载器.exe" .
if ($LASTEXITCODE -eq 0) {
    $f = Get-Item "..\..\dist\Xlauch 下载器.exe"
    "BUILD OK: {0:N2} MB  {1}" -f ($f.Length / 1MB), $f.LastWriteTime
} else {
    "BUILD FAILED"
    exit 1
}
