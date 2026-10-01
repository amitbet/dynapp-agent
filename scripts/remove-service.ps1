# Removes the machine-wide DynApp agent service and everything it added:
# the service, its firewall rules, C:\Program Files\DynApp, the machine PATH
# entry, and the machine-wide dynapp:// handler. Asks for Administrator once
# (UAC) when needed. Agent data in the state directory is kept.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/remove-service.ps1 | iex

$ErrorActionPreference = 'Stop'

# A function, so `return` stays local when another script runs this one
# through Invoke-Expression.
function Remove-DynAppService {
  $Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
  $ServiceName = 'dynapp-shell-agent'

  function Test-Administrator {
    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
  }

  function Get-ProgramFilesDir {
    if ($env:ProgramW6432) { return $env:ProgramW6432 }
    return $env:ProgramFiles
  }

  function Test-ServiceInstallPresent {
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) { return $true }
    if (Test-Path (Join-Path (Get-ProgramFilesDir) 'DynApp\dynapp-shell-agent.exe')) { return $true }
    return (Test-Path 'Registry::HKEY_LOCAL_MACHINE\Software\Classes\dynapp')
  }

  if (-not (Test-ServiceInstallPresent)) {
    Write-Host 'No machine-wide DynApp agent service is installed.'
    return
  }

  if (-not (Test-Administrator)) {
    Write-Host 'Removing the machine-wide DynApp agent service needs Administrator once. Approve the Windows prompt...'
    $hostExe = (Get-Process -Id $PID).Path
    $command = "`$env:DYNAPP_AGENT_REPO = '$Repo'; iex (iwr -UseBasicParsing 'https://raw.githubusercontent.com/${Repo}/main/scripts/remove-service.ps1')"
    Start-Process -FilePath $hostExe -Verb RunAs -Wait -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $command)
    # A deleted service can linger as "marked for deletion" for a moment.
    for ($attempt = 0; $attempt -lt 10 -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue); $attempt++) {
      Start-Sleep -Milliseconds 500
    }
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
      throw 'The machine-wide DynApp agent service is still installed (the Administrator step was declined or failed).'
    }
    Write-Host 'Removed the machine-wide DynApp agent service.'
    return
  }

  $imagePath = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName" -ErrorAction SilentlyContinue).ImagePath
  if ($imagePath) { Write-Host "Service command line: $imagePath" }

  Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
  $programDir = Join-Path (Get-ProgramFilesDir) 'DynApp'
  Get-Process -Name 'dynapp-shell-agent' -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -and $_.Path.StartsWith($programDir, [StringComparison]::OrdinalIgnoreCase) } |
    Stop-Process -Force
  # Native tools report "not found" on stderr, which 'Stop' would turn into
  # a terminating error in Windows PowerShell 5.1.
  if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    try { & sc.exe delete $ServiceName | Out-Null } catch { Write-Warning "sc.exe delete: $_" }
  }

  foreach ($rule in @('DynApp Shell Agent (UDP)', 'DynApp Shell Agent LAN')) {
    try { & netsh advfirewall firewall delete rule "name=$rule" | Out-Null } catch { }
  }

  $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if ($machinePath) {
    $kept = @($machinePath -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ine $programDir) })
    [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'Machine')
  }

  Remove-Item -Recurse -Force 'Registry::HKEY_LOCAL_MACHINE\Software\Classes\dynapp' -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 500
  Remove-Item -Recurse -Force $programDir -ErrorAction SilentlyContinue

  Write-Host 'Removed the machine-wide DynApp agent service, its firewall rules, PATH entry, and dynapp:// handler.'
  if ($imagePath -and $imagePath -match '--state-dir\s+"?([^"]+?)"?(\s|$)') {
    Write-Host "Its data is kept in $($Matches[1])."
  }
}

Remove-DynAppService
