# DynApp Agent

Install this on a machine so [DynApps](https://dynapp.io) can use it. It runs
as a Windows service, a macOS LaunchAgent, or a Linux daemon. Hosted apps talk
to it for local files, network, processes, and other approved capabilities.
Without it an app still renders, but those local features stay off.

The binary is still named `dynapp-shell-agent`. Apps keep using the existing
PWA Shell injectables; this process has no app UI. A loopback settings page
reuses Dyner's workspace and remote-environment panels.

Related repositories: [`amitbet/dynapp`](https://github.com/amitbet/dynapp)
(apps and PWA Shell) and [`amitbet/dyner`](https://github.com/amitbet/dyner)
(catalog and hosting).

## Release

GitHub Actions builds signed `dynapp-shell-agent` binaries for macOS, Windows,
and Linux (amd64 and arm64) and attaches them to a GitHub release.

```sh
# from the Actions tab, or:
gh workflow run release
# or stamp an explicit version:
git tag v0.1.0 && git push origin v0.1.0
```

Leave the workflow version blank to patch-bump the latest `v*` tag. First
release is `0.1.0` unless you pass a version or push a tag. Each asset is
published with `.sha256` and `.sig` sidecars the agent updater verifies.

## Run

The agent starts a loopback HTTP/WebSocket endpoint on port 9011 for local PWA
services, plus a certificate-pinned WebTransport listener for direct access.
The loopback endpoint also serves health, native launcher control, and settings.
Sign in to Dyner once; the agent then creates a device credential
(`POST /api/v1/remote-environments` with the account token from
`DynApp/dyner/auth.json`) and names the environment after this hostname.

```sh
go run ./cmd/dynapp-shell-agent
```

Expose the same listener on the LAN with one flag or env var (no address
required). The default name is still this hostname:

```sh
dynapp-shell-agent --enable-lan
# or
DYNAPP_ENABLE_LAN=1 dynapp-shell-agent
```

Install, start, and stop it as an OS service:

```sh
dynapp-shell-agent install
dynapp-shell-agent start
dynapp-shell-agent stop
dynapp-shell-agent uninstall
```

Installation is a separate explicit action; development and tests never install
an OS service.

Optional overrides, all defaulted when omitted:

| Flag / env | Default |
| --- | --- |
| `--address` | `127.0.0.1:9011` |
| `--dyner-url`, `DYNER_BASE_URL`, `DYNAPP_DYNER_URL` | hosted Dyner (`https://dynapp.io`) |
| `--state-dir` | `~/Library/Application Support/DynApp/shell-agent` (macOS) |
| `--enable-lan`, `DYNAPP_ENABLE_LAN` | off (loopback only) |
| `--relay` | off |
| `--device-credential` | created automatically from the signed-in Dyner account |
| `DYNAPP_DYNER_TOKEN` | Electron `dyner/auth.json` if present |

Settings UI: `http://127.0.0.1:9011/settings`

Runtime logs are appended to `logs/agent.log` beneath the state directory. An
interactive launch also mirrors them to stdout. If the default UDP/TCP port is
already occupied on a first no-argument launch, the agent selects an available
loopback port, persists it, and registers that endpoint with Dyner so the PWA
Shell can still present it as **This machine**.

## Linux Chromium PWA shortcuts

On container/webtop desktops, Chromium writes PWA `.desktop` files without
`--no-sandbox`, so the shortcut exits immediately. When the agent is running
inside a container it watches `~/Desktop` and
`~/.local/share/applications` and inserts `--no-sandbox` into those `Exec=`
lines.

## Tray and global shortcuts

The hosted PWA Shell exposes `appShell.tray` and `appShell.globalShortcut`
through the local agent. Apps declare `tray.manage` and `globalShortcut` in
`backendPermissions`; the agent enforces the approved grant before launching
a helper or handling a command.

Each authenticated app connection owns a native helper, tray icon, and up to
64 hotkeys. Windows service installs launch the helper in the active WTS user
session using that user's token and environment. Interactive Windows launches
use the current session. macOS uses the existing per-user LaunchAgent and
requires a CGO build with AppKit and Carbon. A root LaunchDaemon is not a
supported macOS UI host. Linux and macOS builds with CGO disabled omit these
capabilities from their advertised list.

The helper runs the same binary in a private mode. Its loopback connection
uses a random single-use credential delivered through the child environment,
not its command line. It cannot invoke privileged agent operations. Normal
disconnect, permission revocation, shutdown, and parent failure close the
channel and remove native resources. Apps re-register after reconnecting.
Multiple app windows have separate helpers, matching the Electron contract;
the OS rejects conflicting exclusive hotkeys.

Menus support normal items, separators, checkboxes, radio groups, and nested
submenus. Menus are limited to 128 items and five submenu levels. Icons use
the generic DynApp symbol. `setTitle` displays text on macOS and is a no-op on
Windows. `displayBalloon` uses a Windows balloon or a macOS notification,
subject to the user's notification settings. macOS notification text is
passed as arguments to the system notification command.

Run the native lifecycle smoke test in a signed-in desktop session:

```sh
go build -o /tmp/dynapp-shell-agent ./cmd/dynapp-shell-agent
node ./scripts/smoke-native-presentation.mjs /tmp/dynapp-shell-agent
```

This creates temporary tray icons, checks real OS hotkey conflicts across two
helper processes, and verifies that disconnect releases the registrations.
Windows service-to-user launch still needs verification on a Windows service
install; cross-compilation alone cannot exercise the WTS hand-off.

## Desktop screenshots

Hosted apps declare `screen.capture` and call `appShell.screen.listDisplays()`
or `appShell.screen.capture({ displayId, region })`. The helper captures PNG
bytes on the interactive desktop: ScreenCaptureKit on macOS 14+ (with Screen
Recording permission) and GDI on Windows. Captures are capped at 32 megapixels
and 8 MiB encoded. Region coordinates are device pixels inside the selected
display. Linux and macOS builds with CGO disabled omit this capability.

## Self-updates

Release builds poll this GitHub repository at startup and once per hour. They
select the binary for the current OS and CPU, download it into the agent state
directory, and verify the release's SHA-256 sidecar before using it.

The macOS release binaries are signed with the persistent `DynApp Local
Signing` identity and carry the stable identifier
`com.amitbet.dynapp.shell-agent`. The updater verifies that signature before it
stages the update. Keep the signing `.p12` backed up. Replacing it creates a new
macOS code requirement and can make previously granted permissions appear lost.

The agent counts TCP, UDP, and RDP bridges across local and relay sessions. It
does not replace itself while any bridge is active. Once the count reaches zero,
it rejects new bridge opens, stops the listener, and lets a helper replace and
restart the executable. This keeps the connection-free interruption short and
works on Windows, where a running executable cannot be overwritten.

Development builds do not poll because they carry the `dev` version. Use
`--no-self-update` to disable checks for a release build, or
`--update-repository owner/repository` to point at another GitHub repository.

`enroll` and `configure-lan` remain available for scripted setups. They are not
required for the default path above.

Installing the service with LAN mode enabled also provisions the inbound
firewall permission for the agent. Windows scopes the rule to the
`dynapp-shell-agent` service and its configured UDP listener port. macOS adds
the signed executable to Application Firewall. Uninstalling removes the rule
when the platform supports it. Loopback-only installs do not change firewall
settings.

## Verified contract

The Go integration test executes the protocol that the PWA client uses:

- local `hello` authentication and local-environment descriptor;
- remote-environment protocol v2 negotiation, with a v1 compatibility path;
- filesystem `home`, `roots`, `list`, `stat`, text/base64/chunk reads and
  writes, `mkdir`, `copy`, `move`, `remove`, trash, open/open-with, ZIP,
  recursive watch snapshots, `dirSize`, and `diskUsage`;
- collected and managed streaming process lifecycle, stdin, signals, and exit;
- capability-gated TCP and specialized RDP bridges, including a modern-first
  TLS 1.0/RSA-CBC and TLS-security fallback for legacy Windows RDP hosts;
- rejection of a non-local handshake.

Run it with:

```sh
go test ./...
go vet ./...
go build ./cmd/dynapp-shell-agent
```

On Windows, build with CGo and a C++ toolchain enabled to include the native
OLE clipboard provider. A `CGO_ENABLED=0` build keeps the older eager file
clipboard fallback.

The sibling DynApp repository still runs `tests/go-shell-agent-compat.test.mjs`:
it starts this agent and exercises it through the JavaScript PWA client. That
is the cross-runtime parity test for the implemented transport.

## Parity status

The reusable privileged providers used by current hosted apps are implemented:

| Service family | Status |
| --- | --- |
| Local environment filesystem and `execFile` protocol | Implemented and integration-tested |
| Protocol v2 direct access over authenticated LAN WebTransport | Implemented and integration-tested |
| Local `net.tcp.connect` bridge protocol | Implemented and integration-tested |
| Native binary remote frames / binary write frames | Implemented and integration-tested |
| Pairing-bound browser keys, TLS/WebTransport reliable streams, origin binding, and revocation | Implemented and integration-tested |
| Dyner credential storage and non-interactive enrollment | Implemented and tested |
| Dyner device-authenticated relay-ticket retrieval | Implemented and tested |
| Account relay WebSocket lifecycle, paired-browser session isolation, and E2EE | Implemented and integration-tested |
| Workspace persistence/sync and app updates | PWA/Dyner-owned and already tested outside the agent |
| Managed streaming processes (`stdin`, stdout/stderr events, signal/close) | Implemented and integration-tested |
| Codex and Claude agent sessions with app MCP tools | Implemented and fixture-tested |
| Secrets, file search, calendar, HTTP, FTP/SFTP, durable sessions, system services | Implemented and focused-tested. HTTP fetches require the app's declared `connect` origins or path prefixes. |
| OS file-association launcher fallback and queued delivery | Implemented and focused-tested |
| Local app drafts, DyMaker handoff, and loopback live preview | Implemented and focused-tested. First Edit installs Node.js LTS into the agent state directory when the machine has no `node`/`npm`. |
| Agent-backed lazy clipboard promises, including explicit destination requests from hosted apps | Implemented and focused-tested; Windows uses an OLE `IDataObject` with streamed `FileContents` |
| Window/tray/global-shortcut/screen-capture presentation | Tray, hotkeys, and PNG screenshots on Windows/macOS; Electron `window.manage` retained |

Browser-native outbound file drag cannot be reproduced by a headless process;
`startDrag()` remains a feature probe returning `false`, and Commander uses its
existing download fallback.
