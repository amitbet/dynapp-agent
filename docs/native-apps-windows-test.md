# Native apps on Windows: test plan

The Windows native-app host has only been cross-compiled and unit-tested
off Windows. It stays off unless the agent runs with `DYNAPP_NATIVE_APPS=1`
or `"nativeAppsEnabled": true` in its `config.json`. Run this plan on a real
Windows 10 or 11 machine before turning it on by default (`nativeRequiresOptIn`
in `shellagent/native_platform_windows.go`).

The macOS side is covered by `go test -tags nativee2e ./shellagent/`; the
contract is `docs/native-host.md` in the dynapp repository.

## 1. Unit tests

```powershell
go test ./shellagent/ -run 'Native|PNGToICO|WindowsApp|EncodedPowerShell|DamagedNative'
go vet ./shellagent/winhost/
```

## 2. Per-user agent with the preview on

```powershell
go build -o "$env:LOCALAPPDATA\Programs\DynApp\dynapp-shell-agent.exe" .
[Environment]::SetEnvironmentVariable('DYNAPP_NATIVE_APPS', '1', 'User')
$env:DYNAPP_NATIVE_APPS = '1'
& "$env:LOCALAPPDATA\Programs\DynApp\dynapp-shell-agent.exe" --background
[System.IO.Directory]::GetFiles('\\.\pipe\') | Select-String dynapp-native-host
```

Expect `\\.\pipe\dynapp-native-host-<username>`, and `logs\agent.log` in the
state directory with no native host errors. Also run
`scripts/install-user.ps1` once against a published release to check the
per-user installer, its Run key, and that no console window stays open after
sign-in.

## 3. Install from Dyner

Open `https://dynapp.io` in Edge or Chrome, pick an app that uses the agent
(Notepad.js or Commander.js), and choose **Install**.

- [ ] The Install button appears (Dyner got `nativeSupport.supported`).
- [ ] The permission dialog opens; after allowing, the app window opens.
- [ ] `Start menu ▸ DynApp ▸ <Name>` exists and shows the app icon.
- [ ] `%LOCALAPPDATA%\DynApp\Apps\<owner>\<slug>\icon.ico` exists.

## 4. Window and taskbar identity

- [ ] The window has the app name and icon. No console window remains (a
      minimized console may blink on the taskbar when started from the
      Start menu).
- [ ] The taskbar shows a separate entry for the app, not grouped with Edge or
      another DynApp.
- [ ] Pin it to the taskbar, close it, and start it from the pin: it opens
      the same app (the shortcut and process share `DynApp.<owner>.<slug>`).
- [ ] Starting it again while it runs focuses the existing window.

## 5. Bridge and capabilities

- [ ] The app reaches "This device": open and save a file, list a folder, use
      the clipboard, and drop a file from Explorer (the agent overlay still
      handles Windows drops).
- [ ] An ungranted capability fails with "Review it under Permissions… in the
      window menu."
- [ ] Right-click the title bar: **Permissions…** shows one message box per
      group, defaulting to No. Changing a group reloads the page with the new
      grant.
- [ ] A link to another site opens in the default browser and the app stays
      on its own page.

The riskiest code to watch: `documentSource` (calls `ICoreWebView2::get_Source`
through vtable slot 4), the reflection in `chromiumOf`/`unexportedField` into
`go-webview2`, the system-menu window procedure, and the PowerShell C#
shortcut writer. If bridge messages are ignored, log `documentSource` first.

## 6. Permission prompts

In Dyner → Permissions, revoke the app's grant (`native:<owner/slug>`), then
start the app.

- [ ] A message box lists the ordinary groups with reasons; **No** is the
      default button.
- [ ] Each very-high group (for example Secrets) gets its own message box,
      also defaulting to No.
- [ ] The decision shows up in Dyner → Permissions.

## 7. Without the agent, and uninstall

- [ ] Stop the agent and start the app: the agent is started, or the app
      loads as a plain web app after about 5 seconds.
- [ ] Dyner → app page → **Uninstall** removes the shortcut and icon; the
      `WebView2` data folder stays.

## 8. Machine service

Repeat steps 3 and 4 with the LocalSystem service (`install.ps1`) and
`DYNAPP_NATIVE_APPS=1` in the service environment
(`HKLM\SYSTEM\CurrentControlSet\Services\dynapp-shell-agent`, `Environment`
value). The service launches app windows on the signed-in user's desktop
through `CreateProcessAsUser`, and uses `\\.\pipe\dynapp-native-host`.

- [ ] Install and Open from Dyner show the window on the user's desktop, not
      in session 0.
- [ ] The shortcut and icon land in the user's profile, not the system
      profile.
