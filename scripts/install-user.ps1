# Installs the latest DynApp agent for the current user only: no Administrator
# rights, no machine service, and no SmartScreen prompt (the script downloads
# the release itself, so the binary carries no browser download mark). The
# agent starts at sign-in and updates itself from GitHub releases.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/install-user.ps1 | iex
#
# Set $env:DYNAPP_NATIVE_APPS = '1' before running to also turn on the native
# Windows apps preview, and $env:DYNAPP_AGENT_VERSION to pin a release.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
$BinaryName = 'dynapp-shell-agent.exe'
$RunKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$RunValue = 'DynApp Agent'

function Get-NativeArch {
  $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  switch -Regex ($arch) {
    'ARM64' { return 'arm64' }
    'AMD64' { return 'amd64' }
    default { throw "unsupported architecture $arch" }
  }
}

# The machine-wide service and a per-user agent would compete for the same
# local port and pairings.
# Remove an existing machine service first (one UAC prompt); its state
# directory is normally this user's, so pairings carry over.
if ((Get-Service -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue) -or
    (Test-Path 'Registry::HKEY_LOCAL_MACHINE\Software\Classes\dynapp')) {
  Write-Host 'Found the machine-wide DynApp agent service; replacing it with a per-user agent.'
  Invoke-RestMethod "https://raw.githubusercontent.com/${Repo}/main/scripts/remove-service.ps1" | Invoke-Expression
  if (Get-Service -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue) {
    throw 'The machine-wide DynApp agent service could not be removed, so the per-user agent was not installed.'
  }
}

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$arch = Get-NativeArch
# $env:DYNAPP_AGENT_VERSION = '0.1.33' installs that release (also a
# prerelease) instead of the latest one.
$releaseUrl = if ($env:DYNAPP_AGENT_VERSION) {
  "https://api.github.com/repos/${Repo}/releases/tags/v$($env:DYNAPP_AGENT_VERSION.TrimStart('v'))"
} else {
  "https://api.github.com/repos/${Repo}/releases/latest"
}
$release = Invoke-RestMethod -Uri $releaseUrl -Headers @{ 'User-Agent' = 'dynapp-agent-installer' }
$tag = [string]$release.tag_name
if (-not $tag) { throw 'could not resolve the latest GitHub release' }
$version = $tag.TrimStart('v')
$assetName = "dynapp-shell-agent-${version}-windows-${arch}.exe"
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
$hashAsset = $release.assets | Where-Object { $_.name -eq "${assetName}.sha256" } | Select-Object -First 1
if (-not $asset -or -not $hashAsset) { throw "release $tag has no $assetName and checksum" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) $assetName
$tmpHash = "${tmp}.sha256"
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $tmp -UseBasicParsing
Invoke-WebRequest -Uri $hashAsset.browser_download_url -OutFile $tmpHash -UseBasicParsing
$expected = ((Get-Content -Path $tmpHash -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLowerInvariant()
if ($expected -ne $actual) { throw "checksum mismatch for $assetName" }

$installDir = Join-Path $env:LOCALAPPDATA 'Programs\DynApp'
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$destination = Join-Path $installDir $BinaryName

# Stop a running per-user agent from this folder before replacing it.
Get-Process -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue |
  Where-Object { $_.Path -and ($_.Path -ieq $destination) } |
  Stop-Process -Force
Start-Sleep -Milliseconds 500
Copy-Item -Force -Path $tmp -Destination $destination
Remove-Item -Force -Path $tmp, $tmpHash -ErrorAction SilentlyContinue

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
$parts = @($userPath -split ';' | Where-Object { $_ -and $_.Trim() })
if ($parts -notcontains $installDir) {
  [Environment]::SetEnvironmentVariable('Path', (($parts + $installDir) -join ';'), 'User')
}
if ($env:Path -notlike "*${installDir}*") { $env:Path = "$env:Path;$installDir" }

if ($env:DYNAPP_NATIVE_APPS -eq '1') {
  [Environment]::SetEnvironmentVariable('DYNAPP_NATIVE_APPS', '1', 'User')
}

New-Item -Path $RunKey -Force | Out-Null
Set-ItemProperty -Path $RunKey -Name $RunValue -Value "`"$destination`" --background"
Start-Process -FilePath $destination -ArgumentList '--background' -WindowStyle Hidden

Write-Host "installed $destination from $tag for $env:USERNAME"
Write-Host 'The agent is running and starts automatically when you sign in.'
Write-Host "To remove it: irm https://raw.githubusercontent.com/${Repo}/main/scripts/uninstall-user.ps1 | iex"
