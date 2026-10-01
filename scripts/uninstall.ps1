# Removes the DynApp agent from this computer for the current user: the
# running agent and native app windows, the sign-in start, the dynapp://
# handler, the PATH entry, the native-apps setting, installed native app
# shortcuts, and the program files. If the machine-wide service is installed
# it is removed too, after one UAC prompt.
#
# Pairings, permission grants, and native app data are kept so a reinstall
# picks them up. Set $env:DYNAPP_REMOVE_DATA = '1' to delete them as well.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/uninstall.ps1 | iex

$ErrorActionPreference = 'Stop'
$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\DynApp'
$stateDir = Join-Path $env:APPDATA 'DynApp\shell-agent'
$appsDir = Join-Path $env:LOCALAPPDATA 'DynApp'
$startMenuDir = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\DynApp'

# The agent and every native app window are this executable.
Get-Process -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue |
  Where-Object { $_.Path -and $_.Path.StartsWith($installDir, [StringComparison]::OrdinalIgnoreCase) } |
  Stop-Process -Force
Start-Sleep -Milliseconds 500

Remove-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'DynApp Agent' -ErrorAction SilentlyContinue
Remove-Item -Recurse -Force 'HKCU:\Software\Classes\dynapp' -ErrorAction SilentlyContinue

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath) {
  $kept = @($userPath -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ine $installDir) })
  [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
}
[Environment]::SetEnvironmentVariable('DYNAPP_NATIVE_APPS', $null, 'User')

Remove-Item -Recurse -Force $startMenuDir -ErrorAction SilentlyContinue
Remove-Item -Recurse -Force $installDir -ErrorAction SilentlyContinue
if (Test-Path $installDir) {
  Write-Warning "Could not delete $installDir; a DynApp process may still be running. Sign out or reboot, then delete it."
}

if ($env:DYNAPP_REMOVE_DATA -eq '1') {
  Remove-Item -Recurse -Force $stateDir, $appsDir -ErrorAction SilentlyContinue
  Write-Host "Deleted agent data ($stateDir) and native app data ($appsDir)."
} else {
  # Native app icons go with their shortcuts; their web data stays.
  Get-ChildItem -Path (Join-Path $appsDir 'Apps') -Recurse -Filter 'icon.ico' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  Write-Host "Kept agent data in $stateDir and native app data in $appsDir (set DYNAPP_REMOVE_DATA=1 to delete them)."
}

Write-Host 'Removed the per-user DynApp agent: sign-in start, dynapp:// handler, PATH entry, native app shortcuts, and program files.'

Invoke-RestMethod "https://raw.githubusercontent.com/${Repo}/main/scripts/remove-service.ps1" | Invoke-Expression
