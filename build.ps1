param(
  [string]$Command = "build"
)

$root = $PSScriptRoot
$go   = Join-Path $root "tools\go\bin\go.exe"
$src  = Join-Path $root "code\tdm"
$dist = Join-Path $root "dist"
$out  = Join-Path $dist "XDM.exe"
$cmd  = $Command.TrimStart("-").ToLower()

if (-not (Test-Path $go)) {
  Write-Host "[ERROR] 未找到 Go 工具链: $go" -ForegroundColor Red
  exit 1
}
if (-not (Test-Path $dist)) {
  New-Item -ItemType Directory -Path $dist | Out-Null
}

function Get-SigningCert {
  $cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
    Where-Object { $_.Subject -eq "CN=Xlauch" -and $_.NotAfter -gt (Get-Date) } |
    Sort-Object NotAfter -Descending | Select-Object -First 1
  if ($cert) { return $cert }

  Write-Host "[CERT] 创建本地代码签名证书（CN=Xlauch）..."
  $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject "CN=Xlauch" `
    -FriendlyName "Xlauch Code Signing" -CertStoreLocation Cert:\CurrentUser\My `
    -KeyUsage DigitalSignature -KeyExportPolicy Exportable -NotAfter (Get-Date).AddYears(10)
  $cer = Join-Path $env:TEMP "xlauch-codesign.cer"
  Export-Certificate -Cert $cert -FilePath $cer -Force | Out-Null
  Import-Certificate -FilePath $cer -CertStoreLocation Cert:\CurrentUser\Root | Out-Null
  Import-Certificate -FilePath $cer -CertStoreLocation Cert:\CurrentUser\TrustedPublisher | Out-Null
  Remove-Item $cer -Force
  Write-Host "[CERT] 已加入当前用户「受信任根/受信任发布者」"
  return $cert
}

function Invoke-Sign {
  param([string]$Path)
  try {
    $cert = Get-SigningCert
    $sig = Set-AuthenticodeSignature -FilePath $Path -Certificate $cert -HashAlgorithm SHA256
    if ($sig.Status -eq "Valid") {
      Write-Host "[OK] 已签名，发布者: Xlauch" -ForegroundColor Green
    } else {
      Write-Host "[WARN] 签名状态: $($sig.Status) $($sig.StatusMessage)" -ForegroundColor Yellow
    }
  } catch {
    Write-Host "[WARN] 签名失败: $_" -ForegroundColor Yellow
  }
  Unblock-File -Path $Path -ErrorAction SilentlyContinue
}

# 浏览器扩展为原生 JS（MV3），无需编译：同步源码到 go:embed 目录并打包分发 zip。
# 内嵌目录用于 XDM 内置的「帮助 → 插件下载与使用」一键解压；dist zip 用于官网/手动分发。
function Invoke-ExtensionPackage {
  $extSrc = Join-Path $root "code\browser-extension"
  $extDst = Join-Path $src "internal\server\webext"
  $extZip = Join-Path $dist "xdm-browser-extension.zip"

  if (-not (Test-Path (Join-Path $extSrc "manifest.json"))) {
    Write-Host "[ERROR] 扩展源码缺失: $extSrc\manifest.json" -ForegroundColor Red
    return $false
  }

  # 1) 同步到内嵌目录（编译期由 //go:embed all:webext 读取）
  if (Test-Path $extDst) { Remove-Item $extDst -Recurse -Force }
  Copy-Item $extSrc $extDst -Recurse -Force
  $n = (Get-ChildItem $extDst -File -Recurse).Count
  Write-Host "[OK] 扩展已同步到内嵌目录 webext\（$n 个文件）" -ForegroundColor Green

  # 2) 打包分发 zip（内容为扩展文件，manifest.json 位于压缩包根）
  try {
    if (Test-Path $extZip) { Remove-Item $extZip -Force }
    Compress-Archive -Path (Join-Path $extSrc "*") -DestinationPath $extZip -Force -ErrorAction Stop
    $entries = (Get-ChildItem $extSrc -File -Recurse).Count
    $kb = [math]::Round((Get-Item $extZip).Length / 1KB, 1)
    Write-Host "[OK] 扩展包已更新: $extZip（$entries 个文件，$kb KB）" -ForegroundColor Green
  } catch {
    Write-Host "[WARN] 扩展包打包失败（可能被占用，请关闭资源管理器预览后重试）: $_" -ForegroundColor Yellow
    return $false
  }
  return $true
}

function Invoke-Build {
  Write-Host "[EXT] 准备浏览器扩展（同步内嵌目录 + 打包 zip）"
  Invoke-ExtensionPackage | Out-Null

  Write-Host "[BUILD] $out"
  Push-Location $src
  try {
    & $go build -trimpath -ldflags="-s -w -H windowsgui" -o $out .
    $code = $LASTEXITCODE
  } finally {
    Pop-Location
  }
  if ($code -ne 0) {
    Write-Host "[FAILED] 编译失败，请确认程序已退出且文件未被占用" -ForegroundColor Red
  } else {
    Write-Host "[OK] 编译完成" -ForegroundColor Green
    Invoke-Sign $out
  }
  return $code
}

switch ($cmd) {
  "test" {
    Push-Location $src
    try {
      & $go test ./...
      $code = $LASTEXITCODE
    } finally {
      Pop-Location
    }
    if ($code -eq 0) { Write-Host "[OK] 测试通过" -ForegroundColor Green }
    exit $code
  }
  "run" {
    $code = Invoke-Build
    if ($code -ne 0) { exit $code }
    Start-Process -FilePath $out
    exit 0
  }
  "clean" {
    if (Test-Path $out) { Remove-Item $out -Force }
    Write-Host "[OK] 已清理编译产物"
    exit 0
  }
  "help" {
    Write-Host "用法: build.bat [test|run|clean]"
    Write-Host "  (无参数)  编译并签名到 dist\XDM.exe"
    Write-Host "  test      运行 go test ./..."
    Write-Host "  run       编译、签名并启动程序"
    Write-Host "  clean     删除编译产物"
    exit 0
  }
  default {
    $code = Invoke-Build
    exit $code
  }
}
