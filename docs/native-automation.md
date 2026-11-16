# Native app automation

A coding agent on this computer (Codex, Claude Code, a script) can drive a DynApp
that runs as a native desktop app: run JavaScript in its page, read console
output, take screenshots, click and type with real input events, navigate,
reload, and move or resize the window. Native apps run in WKWebView (macOS) or
WebView2 (Windows), which Playwright and other CDP tools cannot attach to. This
channel fills that gap. It goes through the agent that already hosts the app.

```text
coding agent ── CLI or MCP ──▶ automation socket ──▶ dynapp-shell-agent
                                                         │ (checks consent)
                                                         ▼
                              app host (Swift / WebView2) ◀── control channel
                                       │
                                       ▼
                                 page (main world)
```

## Who can use it

- **Local only.** The automation socket is a Unix socket
  (`<stateDir>/automation.sock`, mode `0600`, next to `native-host.sock`) on
  macOS, and a named pipe (`\\.\pipe\dynapp-automation-<user>`, or
  `\\.\pipe\dynapp-automation` for the machine service) with owner-only access
  on Windows. Web pages, Dyner, the relay and other machines cannot reach it.
- **Per app, by the user.** Automation is off for every app. `enable` makes the
  app's own window ask "Allow coding agents to control <app>?" (Don't Allow is
  the default button). The choice is stored as `nativeApps[].automation` in the
  agent config. A coding agent can send the request but cannot answer it.
- **Visible and revocable.** While automation is on, the macOS window subtitle
  reads "Automation on", and the app menu has **Stop Automation**. On Windows
  the window menu (Alt+Space) has **Stop Automation**. `disable` turns it off too.
  Reinstalling the app clears the choice.
- Any process running as the user can already read the agent's state, so
  consent protects against surprise, not against malware running as the user.

Hosts installed before this feature do not advertise it. On macOS the agent
replaces an older host in an installed bundle once the app has quit (and
before `launch`), so quitting and reopening the app is enough; camera,
microphone and Local Network grants for that app are then asked again.

## Command line

```text
dynapp-shell-agent native apps
dynapp-shell-agent native launch   <app>
dynapp-shell-agent native enable   <app>
dynapp-shell-agent native disable  <app>
dynapp-shell-agent native eval     <app> <code | ->         [--timeout 30s]
dynapp-shell-agent native wait     <app> --selector S | --expr E [--timeout 10s]
dynapp-shell-agent native snapshot <app> [file.png] [--width N] [--window]
dynapp-shell-agent native console  <app> [--follow] [--since N] [--limit N]
dynapp-shell-agent native reload   <app>
dynapp-shell-agent native navigate <app> <url | /path | #/route>
dynapp-shell-agent native click    <app> <selector> | --x X --y Y [--count 2]
dynapp-shell-agent native type     <app> <text> [--selector S]
dynapp-shell-agent native key      <app> <key> [--mods cmd,shift]
dynapp-shell-agent native window   <app> [--width W --height H] [--x X --y Y] [--focus]
dynapp-shell-agent native mcp
```

`<app>` is a store id (`owner/slug`), a slug, or the app's name. `--state-dir`
points at a non-default agent state directory.

- **eval** runs in the page's main world, so app globals and dev hooks
  (`window.__fastlane`, `window.__mdview`) are visible. Code is tried as an
  expression first (`document.title`, `await fetch(...)`); if that does not
  parse, it runs as a function body, so multi-statement code needs `return`.
  Top-level `await` works. The page's CSP does not apply. The result is
  printed as JSON: DOM elements become `"<button#save.primary> \"Save\""`,
  cycles `"[Circular]"`, `undefined` prints as `undefined`. A thrown error
  exits 1 with its message and stack. Variables do not persist between calls;
  assign to `window` to keep something.
- **wait** polls inside the page until the selector matches a visible element
  or the expression is truthy, across page loads.
- **snapshot** writes a PNG of the page (or the whole window with `--window`).
  By default one image pixel is one CSS pixel, so positions read off the image
  can be passed to `click --x --y`.
- **console** prints captured `console.*` output, uncaught errors, unhandled
  rejections and page loads since automation was turned on (the last 2000
  entries per app). Each output ends with a cursor; `--since <cursor>` prints
  only newer entries; `--follow` streams until interrupted.
- **click / type / key** send real macOS input events (`isTrusted` is true), so
  gesture-gated APIs such as clipboard writes and fullscreen work. Clicks go to
  WebKit directly, bypassing the overlay title bar's drag area. Keyboard
  shortcuts go to the page, never to the app's menu (Cmd+Q does not quit).
  `type --selector` clicks the element first to focus it.
- **navigate** accepts URLs and paths on the app's origin only.

## MCP

`dynapp-shell-agent native mcp` serves the same commands as MCP tools over
stdio (`apps`, `launch`, `enable`, `disable`, `eval`, `wait`, `snapshot`,
`console`, `click`, `type`, `key`, `reload`, `navigate`, `window`). `snapshot`
returns an image content block. Register it once:

```sh
codex mcp add dynapp-native -- dynapp-shell-agent native mcp
claude mcp add dynapp-native -- dynapp-shell-agent native mcp
```

## Socket protocol

Newline-delimited JSON. Each request carries an `id`; replies echo it.

```json
{"id":1,"cmd":"eval","app":"fastlane","code":"document.title","timeoutMs":30000}
{"id":1,"ok":true,"result":{"value":"Fastlane"}}
{"id":2,"ok":false,"error":"automation is off for amit-bet/fastlane; …"}
```

Request fields: `cmd`, `app`, `code`, `selector`, `expression`, `url`, `text`,
`key`, `modifiers` (`meta`, `control`, `alt`, `shift`), `x`, `y`, `width`,
`height`, `count`, `window`, `focus`, `since`, `limit`, `follow`, `timeoutMs`.
A `console` request with `follow: true` streams `{"id":…,"event":{seq,time,level,source,text}}`
lines until the client closes the connection. Several requests may be in
flight on one connection.

## Host control frames

The host advertises the feature in its control hello:
`{"type":"native-host-hello",…,"purpose":"control","features":["automation"]}`.
Its `native-host-ready` then carries `"automation": true|false`, and the
bootstrap config carries the same flag.

| Direction | Frame |
| --- | --- |
| agent → host | `{"type":"native-host-automation","id","op",…args}` with `op` = `consent`, `eval` (`script`, `fallback`), `snapshot` (`width`, `window`), `reload`, `navigate` (`url`), `click` (`x`, `y`, `count`), `type` (`text`), `key` (`key`, `modifiers`), `window` (`width`, `height`, `x`, `y`, `focus`) |
| host → agent | `{"type":"native-host-automation-result","id","value"?,"error"?}` |
| agent → host | `{"type":"native-host-automation-state","enabled"}` |
| host → agent | `{"type":"native-host-console","entries":[{level,source,text,time}]}` |
| host → agent | `{"type":"native-host-automation-stop"}` (the user chose Stop Automation) |

The agent builds `script` and `fallback` as async function bodies that return
a JSON string and never throw. The macOS host runs them with
`callAsyncJavaScript(in: .page)`, and runs `fallback` when the first does not
parse. The Windows host runs them with `ExecuteScript` and posts the result
back through the bridge (`op: "automationResult"`). A script that is still
running when its page goes away is failed on the next commit, so `wait`
retries on the new page.

The host adds `window.__DYNAPP_NATIVE_AUTOMATION__ = true` at document start
while automation is on. The page bridge then wraps `console.*` and listens for
`error` and `unhandledrejection`, batching entries to the host with
`{op:"console",entries}`. The wrappers are installed only once automation is
on, so the Web Inspector shows the page's own call sites otherwise.

Synthesized input waits for a script round trip after each event: WebKit
drops input that arrives while the web process is still handling earlier
input.

## Platform status

| | macOS | Windows | Android |
| --- | --- | --- | --- |
| eval, wait, console, reload, navigate, window | yes | yes, not yet run on a real machine | no: use `adb` + WebView CDP on debug builds |
| snapshot, click, type, key | yes | not yet (use `eval` with `element.click()`) | no |

## Tests

```sh
go test -run Automation ./shellagent/                       # agent routing, consent, CLI, MCP
node --test shellagent/native_host_bridge.test.mjs          # console capture
go test -tags nativee2e -run TestNativeAutomation -v ./shellagent/   # macOS: real bundle, WKWebView
```

The macOS e2e test installs a real bundle, grants automation, and drives eval
(under a CSP without `unsafe-eval`), console capture, snapshot, trusted clicks
and keys, window resize, navigation and reload, then checks that `disable`
stops commands and that `enable` shows the consent sheet
(`DYNAPP_AUTOMATION_E2E_KEEP=<dir>` keeps the screenshots).
