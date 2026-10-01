# Installs the per-user DynApp agent with native desktop apps turned on: the
# same as install-user.ps1 with $env:DYNAPP_NATIVE_APPS = '1'. No
# Administrator rights.
#   irm https://raw.githubusercontent.com/amitbet/dynapp-agent/main/scripts/install-native-apps.ps1 | iex

$ErrorActionPreference = 'Stop'
$Repo = if ($env:DYNAPP_AGENT_REPO) { $env:DYNAPP_AGENT_REPO } else { 'amitbet/dynapp-agent' }
$env:DYNAPP_NATIVE_APPS = '1'
# install-user.ps1 saves DYNAPP_NATIVE_APPS for the user and starts the
# agent from this session, so both the running agent and later sign-in starts
# have native apps on.
Invoke-RestMethod "https://raw.githubusercontent.com/${Repo}/main/scripts/install-user.ps1" | Invoke-Expression
