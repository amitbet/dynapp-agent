# Removes the DynApp agent from this computer completely: the per-user agent
# (uninstall-user.ps1) and, after one UAC prompt, the machine-wide service if
# one is installed (remove-service.ps1).
#
# Pairings, permission grants, and native app data are kept. Set
# $env:DYNAPP_REMOVE_DATA = '1' to delete them as well.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/uninstall.ps1 | iex

$ErrorActionPreference = 'Stop'
$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
Invoke-RestMethod "https://raw.githubusercontent.com/${Repo}/main/scripts/uninstall-user.ps1" | Invoke-Expression
Invoke-RestMethod "https://raw.githubusercontent.com/${Repo}/main/scripts/remove-service.ps1" | Invoke-Expression
