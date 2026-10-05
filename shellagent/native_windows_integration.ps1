$ErrorActionPreference = 'Stop'
# Run with the interactive user's token, including when the agent is a service.
$registry = [Microsoft.Win32.Registry]::CurrentUser
$base = 'Software'
$id = $env:DYNAPP_NATIVE_ID
$appPath = "$base\DynApp\NativeApps\$id"
function Set-NativeValue($path, $name, $value) {
  $key = $registry.CreateSubKey($path)
  try { $key.SetValue($name, $value, [Microsoft.Win32.RegistryValueKind]::String) }
  finally { $key.Dispose() }
}
function Remove-NativeAssociations {
  $key = $registry.OpenSubKey("$appPath\Capabilities\FileAssociations")
  if ($null -ne $key) {
    try { $extensions = @($key.GetValueNames()) } finally { $key.Dispose() }
    foreach ($extension in $extensions) {
      $openWith = $registry.OpenSubKey("$base\Classes\$extension\OpenWithProgids", $true)
      if ($null -ne $openWith) {
        try { $openWith.DeleteValue($id, $false) } finally { $openWith.Dispose() }
      }
    }
  }
  $registry.DeleteSubKeyTree("$base\Classes\$id", $false)
  $registry.DeleteSubKeyTree("$appPath\Capabilities", $false)
  $registered = $registry.OpenSubKey("$base\RegisteredApplications", $true)
  if ($null -ne $registered) {
    try { $registered.DeleteValue($id, $false) } finally { $registered.Dispose() }
  }
}
$desktop = [Environment]::GetFolderPath('DesktopDirectory')
$old = $registry.OpenSubKey($appPath)
$oldDesktop = $null
if ($null -ne $old) {
  try { $oldDesktop = $old.GetValue('DesktopShortcut') } finally { $old.Dispose() }
}
Remove-NativeAssociations
if ($env:DYNAPP_NATIVE_MODE -eq 'uninstall') {
  if ($oldDesktop -and ([IO.Path]::GetDirectoryName($oldDesktop) -eq $desktop)) {
    Remove-Item -LiteralPath $oldDesktop -Force -ErrorAction SilentlyContinue
  }
  $registry.DeleteSubKeyTree($appPath, $false)
} else {
  # Resolve the known folder in the user's session: Desktop may be in OneDrive.
  if (-not $desktop) { throw 'Could not find the user desktop folder' }
  $desktopShortcut = Join-Path $desktop ([IO.Path]::GetFileName($env:DYNAPP_LNK_PATH))
  if ($oldDesktop -and $oldDesktop -ne $desktopShortcut -and ([IO.Path]::GetDirectoryName($oldDesktop) -eq $desktop)) {
    Remove-Item -LiteralPath $oldDesktop -Force -ErrorAction SilentlyContinue
  }
  Copy-Item -LiteralPath $env:DYNAPP_LNK_PATH -Destination $desktopShortcut -Force
  Set-NativeValue $appPath 'DesktopShortcut' $desktopShortcut
  $extensions = $env:DYNAPP_NATIVE_EXTENSIONS | ConvertFrom-Json
  if ($extensions.Count -gt 0) {
    Set-NativeValue "$base\Classes\$id" '' ($env:DYNAPP_NATIVE_NAME + ' Document')
    Set-NativeValue "$base\Classes\$id" 'FriendlyTypeName' ($env:DYNAPP_NATIVE_NAME + ' Document')
    Set-NativeValue "$base\Classes\$id\Application" 'ApplicationName' $env:DYNAPP_NATIVE_NAME
    Set-NativeValue "$base\Classes\$id\Application" 'ApplicationIcon' ($env:DYNAPP_LNK_ICON + ',0')
    Set-NativeValue "$base\Classes\$id\DefaultIcon" '' ($env:DYNAPP_LNK_ICON + ',0')
    $command = '"' + $env:DYNAPP_LNK_TARGET + '" ' + $env:DYNAPP_LNK_ARGS + ' -- "%1"'
    Set-NativeValue "$base\Classes\$id\shell\open\command" '' $command
    Set-NativeValue "$appPath\Capabilities" 'ApplicationName' $env:DYNAPP_NATIVE_NAME
    Set-NativeValue "$appPath\Capabilities" 'ApplicationDescription' ($env:DYNAPP_NATIVE_NAME + ' (DynApp)')
    Set-NativeValue "$appPath\Capabilities" 'ApplicationIcon' ($env:DYNAPP_LNK_ICON + ',0')
    foreach ($extension in $extensions) {
      Set-NativeValue "$appPath\Capabilities\FileAssociations" $extension $id
      $openWith = $registry.CreateSubKey("$base\Classes\$extension\OpenWithProgids")
      try { $openWith.SetValue($id, [byte[]]@(), [Microsoft.Win32.RegistryValueKind]::None) }
      finally { $openWith.Dispose() }
    }
    Set-NativeValue "$base\RegisteredApplications" $id "$appPath\Capabilities"
  }
}
# Refresh Explorer's cached file associations.
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class DynAppAssociations {
  [DllImport("shell32.dll")] public static extern void SHChangeNotify(uint eventId, uint flags, IntPtr item1, IntPtr item2);
}
'@
[DynAppAssociations]::SHChangeNotify(0x08000000, 0, [IntPtr]::Zero, [IntPtr]::Zero)
