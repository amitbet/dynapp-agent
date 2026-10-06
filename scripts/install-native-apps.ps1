# Installs the latest DynApp agent for the current user with native desktop
# apps turned on. No Administrator rights,
# except one prompt to remove an older machine-wide service if there is one.
#
# The script only downloads the release, checks its checksum, and runs
# `dynapp-shell-agent.exe install-user`. The agent does the rest (copying
# itself to %LOCALAPPDATA%\Programs\DynApp, the sign-in start, PATH, and
# replacing a service), which keeps antivirus script scanning from treating
# the installer as a dropper. Set $env:DYNAPP_AGENT_VERSION to pin a release.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }

$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if ($identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Warning 'This PowerShell runs as Administrator. The agent should run as you: open a normal PowerShell and run the command again.'
  return
}

$arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch -Regex ($arch) { 'ARM64' { 'arm64' } 'AMD64' { 'amd64' } default { throw "unsupported architecture $arch" } }
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$releaseUrl = if ($env:DYNAPP_AGENT_VERSION) {
  "https://api.github.com/repos/${Repo}/releases/tags/v$($env:DYNAPP_AGENT_VERSION.TrimStart('v'))"
} else {
  "https://api.github.com/repos/${Repo}/releases/latest"
}
$release = Invoke-RestMethod -Uri $releaseUrl -Headers @{ 'User-Agent' = 'dynapp-agent-installer' }
$version = ([string]$release.tag_name).TrimStart('v')
$assetName = "dynapp-shell-agent-${version}-windows-${arch}.exe"
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
$hashAsset = $release.assets | Where-Object { $_.name -eq "${assetName}.sha256" } | Select-Object -First 1
if (-not $asset -or -not $hashAsset) { throw "release $($release.tag_name) has no $assetName and checksum" }

$download = Join-Path ([IO.Path]::GetTempPath()) 'dynapp-shell-agent-setup.exe'
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $download -UseBasicParsing
$expected = ((Invoke-RestMethod -Uri $hashAsset.browser_download_url).Trim() -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash -Algorithm SHA256 -Path $download).Hash.ToLowerInvariant()
if ($expected -ne $actual) { Remove-Item -Force $download; throw "checksum mismatch for $assetName" }

Write-Host "Installing DynApp agent $version..."
$installArgs = @('install-user', '--native-apps')
# A pipeline makes PowerShell wait for a GUI-subsystem executable and capture
# its standard output instead of returning while installation is still running.
& $download @installArgs | Out-Host
$code = $LASTEXITCODE
Remove-Item -Force $download -ErrorAction SilentlyContinue
if ($code -ne 0) { throw "The DynApp agent installer failed (exit code $code)." }
Write-Host "To remove it later, run scripts/uninstall-user.ps1 from https://github.com/${Repo}."
