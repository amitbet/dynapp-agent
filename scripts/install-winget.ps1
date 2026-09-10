# Installs the latest DynApp agent with winget from the repo manifests.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/install-winget.ps1 | iex

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
$winget = Get-Command winget -ErrorAction SilentlyContinue
if (-not $winget) {
  throw 'winget is not installed. Use scripts/install.ps1 instead.'
}

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$files = @(
  'AmitBet.DynAppShellAgent.yaml',
  'AmitBet.DynAppShellAgent.installer.yaml',
  'AmitBet.DynAppShellAgent.locale.en-US.yaml'
)
$dir = Join-Path ([IO.Path]::GetTempPath()) 'dynapp-winget'
New-Item -ItemType Directory -Force -Path $dir | Out-Null

foreach ($name in $files) {
  $urls = @(
    "https://amitbet.github.io/dynapp-agent/winget/${name}",
    "https://raw.githubusercontent.com/${Repo}/main/winget/${name}"
  )
  $destination = Join-Path $dir $name
  $downloaded = $false
  foreach ($url in $urls) {
    try {
      Invoke-WebRequest -Uri $url -OutFile $destination -UseBasicParsing
      $downloaded = $true
      break
    } catch {
      # Try the next URL.
    }
  }
  if (-not $downloaded) {
    throw "could not download $name"
  }
}

& winget install --manifest $dir --disable-interactivity --accept-package-agreements --accept-source-agreements
if ($LASTEXITCODE -ne 0) {
  throw "winget install failed with exit code $LASTEXITCODE"
}

Write-Host 'installed dynapp-shell-agent. Start it with: dynapp-shell-agent install && dynapp-shell-agent start'
