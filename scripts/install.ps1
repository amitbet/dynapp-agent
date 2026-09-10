# Installs the latest DynApp agent into Program Files and the machine PATH.
# Run elevated, or this script will relaunch itself with UAC.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/install.ps1 | iex

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
$ScriptUrl = "https://raw.githubusercontent.com/${Repo}/main/scripts/install.ps1"
$BinaryName = 'dynapp-shell-agent.exe'

function Test-Administrator {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-NativeArch {
  if ($env:PROCESSOR_ARCHITEW6432) {
    $arch = $env:PROCESSOR_ARCHITEW6432
  } else {
    $arch = $env:PROCESSOR_ARCHITECTURE
  }
  switch -Regex ($arch) {
    'ARM64' { return 'arm64' }
    'AMD64' { return 'amd64' }
    default { throw "unsupported architecture $arch" }
  }
}

function Get-ProgramFilesDir {
  if ($env:ProgramW6432) { return $env:ProgramW6432 }
  return $env:ProgramFiles
}

if (-not (Test-Administrator)) {
  Write-Host 'This installer needs Administrator. Relaunching elevated...'
  $hostExe = (Get-Process -Id $PID).Path
  if ($PSCommandPath) {
    Start-Process -FilePath $hostExe -Verb RunAs -Wait -ArgumentList @(
      '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $PSCommandPath
    )
  } else {
    $command = "iex (iwr -UseBasicParsing '$ScriptUrl')"
    Start-Process -FilePath $hostExe -Verb RunAs -Wait -ArgumentList @(
      '-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $command
    )
  }
  return
}

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$arch = Get-NativeArch
$api = "https://api.github.com/repos/${Repo}/releases/latest"
$release = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'dynapp-agent-installer' }
$tag = [string]$release.tag_name
if (-not $tag) { throw 'could not resolve the latest GitHub release' }
$version = $tag.TrimStart('v')
$assetName = "dynapp-shell-agent-${version}-windows-${arch}.exe"
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
$hashAsset = $release.assets | Where-Object { $_.name -eq "${assetName}.sha256" } | Select-Object -First 1
if (-not $asset -or -not $hashAsset) {
  throw "release $tag has no $assetName and checksum"
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) $assetName
$tmpHash = "${tmp}.sha256"
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $tmp -UseBasicParsing
Invoke-WebRequest -Uri $hashAsset.browser_download_url -OutFile $tmpHash -UseBasicParsing

$expected = ((Get-Content -Path $tmpHash -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLowerInvariant()
if ($expected -ne $actual) {
  throw "checksum mismatch for $assetName"
}

$installDir = Join-Path (Get-ProgramFilesDir) 'DynApp'
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$destination = Join-Path $installDir $BinaryName
$service = Get-Service -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue
if ($service -and $service.Status -ne 'Stopped') {
  Stop-Service -Name 'dynapp-shell-agent' -Force
}
Copy-Item -Force -Path $tmp -Destination $destination

$machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if (-not $machinePath) { $machinePath = '' }
$parts = @($machinePath -split ';' | Where-Object { $_ -and $_.Trim() })
if ($parts -notcontains $installDir) {
  $parts += $installDir
  [Environment]::SetEnvironmentVariable('Path', ($parts -join ';'), 'Machine')
}
if ($env:Path -notlike "*${installDir}*") {
  $env:Path = "$env:Path;$installDir"
}

Write-Host "installed $destination from $tag"
Write-Host 'installed dynapp-shell-agent. Start it with: dynapp-shell-agent install && dynapp-shell-agent start'
