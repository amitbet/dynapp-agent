//go:build linux && !android && nativee2e

package shellagent

// End-to-end check of a real installed app on Linux: the agent writes the
// .desktop entry, `gio launch` starts dynapp-app-host through it, WebKitGTK
// loads the page, and the page reaches the agent over the bridge. Needs a
// display, a session bus and a built host:
//
//	CGO_ENABLED=1 go build -tags webkitgtk -o /tmp/dynapp-app-host ./cmd/dynapp-app-host
//	DYNAPP_APP_HOST=/tmp/dynapp-app-host dbus-run-session -- xvfb-run -a \
//	  go test -tags nativee2e -run 'TestNativeLinux' -v ./shellagent/

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

const linuxE2EPage = `<!doctype html><meta charset="utf-8"><title>e2e</title><body>native e2e
<iframe src="__FOREIGN__/frame" width="200" height="80"></iframe>
<script>
const report = (data) => fetch("/report", { method: "POST", body: JSON.stringify(data) });
report({ loaded: true });
// WebKitGTK has no WebTransport; the host's stand-in must carry a LAN
// session: 300 KB out and back through a QUIC echo server.
(async () => {
  const sent = new Uint8Array(300000);
  for (let index = 0; index < sent.length; index += 1) sent[index] = index % 251;
  const hash = Uint8Array.from(atob("__WT_HASH__"), (character) => character.charCodeAt(0));
  const transport = new WebTransport("__WT_URL__", { serverCertificateHashes: [{ algorithm: "sha-256", value: hash }] });
  await transport.ready;
  const { readable, writable } = await transport.createBidirectionalStream();
  const writer = writable.getWriter();
  for (let offset = 0; offset < sent.length; offset += 65536) await writer.write(sent.subarray(offset, offset + 65536));
  await writer.close();
  const reader = readable.getReader();
  let received = 0;
  let same = true;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    for (let index = 0; index < value.length; index += 1) if (value[index] !== (received + index) % 251) same = false;
    received += value.length;
  }
  transport.close();
  report({ webtransport: { ok: same && received === sent.length, received } });
})().catch((error) => report({ webtransport: { ok: false, error: String(error && error.message || error) } }));
// The page owns its keys (Commander: F1-F10, Esc, Alt+F7...); the host's
// accelerators (F5/Ctrl+R reload, F11, F12) must not take them. Ctrl+R
// stays unhandled so the host can still reload.
addEventListener("keydown", (event) => {
  const combo = (event.ctrlKey ? "ctrl+" : "") + (event.altKey ? "alt+" : "") + (event.shiftKey ? "shift+" : "") + event.key;
  if (combo === "ctrl+r") return;
  event.preventDefault();
  report({ key: combo });
});
(async () => {
  const host = window.__DYNAPP_NATIVE_HOST__;
  if (!host) return report({ ok: false, error: "window.__DYNAPP_NATIVE_HOST__ is missing" });
  if (!host.linuxTitlebar || host.titlebar) throw new Error("Linux title bar bridge was not advertised separately");
  host.linuxTitlebar.setEnabled(true);
  const captionStyle = document.documentElement.style;
  for (let attempt = 0; !captionStyle.getPropertyValue("--dynapp-linux-caption-scheme"); attempt++) {
    if (attempt >= 100) throw new Error("GTK did not supply the title bar palette");
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  if (!captionStyle.getPropertyValue("--dynapp-linux-caption-width") ||
      !CSS.supports("color", captionStyle.getPropertyValue("--dynapp-linux-caption-bg")) ||
      !CSS.supports("color", captionStyle.getPropertyValue("--dynapp-linux-caption-fg"))) throw new Error("GTK title bar styling is incomplete");
  const socket = host.openAgentSocket();
  const frames = [];
  socket.addEventListener("message", (event) => frames.push(typeof event.data === "string" ? JSON.parse(event.data) : { binaryBytes: event.data.byteLength }));
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("close", (event) => reject(new Error("closed " + event.code + " " + event.reason)), { once: true });
  });
  const wait = async (match) => {
    for (let attempt = 0; attempt < 200; attempt += 1) {
      const frame = frames.find(match);
      if (frame) return frame;
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    throw new Error("timed out; frames: " + JSON.stringify(frames));
  };
  socket.send(JSON.stringify({ type: "hello", protocol: 2 }));
  const hello = await wait((frame) => frame.type === "hello");
  socket.send(JSON.stringify({ type: "ping", id: "p1" }));
  const pong = await wait((frame) => frame.type === "pong");
  socket.send(JSON.stringify({ type: "fs", id: "f1", method: "readText", args: [__FILE__] }));
  const read = await wait((frame) => frame.id === "f1");
  socket.send(JSON.stringify({ type: "exec", id: "e1", command: "echo hi" }));
  const exec = await wait((frame) => frame.id === "e1");
  let openSequence = 0;
  let polling = false;
  setInterval(async () => {
    if (polling) return;
    polling = true;
    try {
      const id = "open" + (++openSequence);
      socket.send(JSON.stringify({ type: "rpc", id, service: "externalOpen", method: "takeData", args: [] }));
      const result = await wait((frame) => frame.id === id);
      if (result.error) report({ opened: false, error: result.error });
      for (const record of result.result || []) report({ opened: record });
    } finally { polling = false; }
  }, 200);
  const reloaded = sessionStorage.getItem("reloaded") === "1";
  await report({ ok: true, reloaded, platform: host.platform, storeId: host.storeId, capabilities: hello.capabilities, pong: pong.type, read, exec });
  if (!reloaded) {
    sessionStorage.setItem("reloaded", "1");
    location.reload();
    return;
  }
  // A script navigation away from the app must not replace the app page.
  setTimeout(() => { location.href = "__FOREIGN__/escape"; }, 300);
  setTimeout(() => report({ stayed: location.origin }), 2500);
})().catch((error) => report({ ok: false, error: String(error && error.message || error) }));
</script>`

// The iframe is another origin. It must load, and its forged bridge
// messages must be dropped.
const linuxE2EFrame = `<!doctype html><body>frame<script>
const forged = { dynappNativeHost: 1, op: "open", sid: "forged" };
let posted = false;
try { window.webkit.messageHandlers.dynappNativeHost.postMessage(JSON.stringify(forged)); posted = true; } catch (error) {}
fetch("/frame-loaded?posted=" + posted + "&host=" + (typeof window.__DYNAPP_NATIVE_HOST__));
</script>`

func TestNativeLinuxAppEndToEnd(t *testing.T) {
	host := os.Getenv("DYNAPP_APP_HOST")
	if host == "" {
		t.Skip("set DYNAPP_APP_HOST to a built dynapp-app-host")
	}
	t.Setenv("DYNAPP_NATIVE_APPS", "1")
	stateDir, err := os.MkdirTemp("/tmp", "dnh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	home := filepath.Join(stateDir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	secret := filepath.Join(stateDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("hello from the agent"), 0o600); err != nil {
		t.Fatal(err)
	}

	foreignEvents := make(chan string, 16)
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case foreignEvents <- r.URL.RequestURI():
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, linuxE2EFrame)
	}))
	defer foreign.Close()

	reports := make(chan map[string]any, 16)
	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/apps/e2e/probe", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"app":           map[string]any{"id": "e2e/probe", "name": "Native Probe"},
			"hostedOrigins": []string{origin},
			"latestRevision": map[string]any{"id": "rev_1", "manifest": map[string]any{
				"name":   "Native Probe",
				"launch": map[string]any{"fileTypes": []any{map[string]any{"name": "Text", "extensions": []any{"txt"}, "role": "Editor"}}},
				"backendPermissions": []any{
					map[string]any{"permission": "externalOpen.files", "reason": "Open files from the file manager."},
					map[string]any{"permission": "fs.readText", "reason": "Reads the probe file to prove the bridge works end to end."},
					map[string]any{"permission": "fs.exec", "reason": "Declared but not granted, to prove denials reach the page."},
				},
			}},
		})
	})
	icon := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(linuxTestPNG(t, color.RGBA{30, 160, 90, 255}))
	}
	mux.HandleFunc("/icon-512.png", icon)
	mux.HandleFunc("/icon-512-maskable.png", icon)
	mux.HandleFunc("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"start_url": "/?launch=pwa"})
	})
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		var report map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &report)
		select {
		case reports <- report:
		default:
		}
	})
	wtURL, wtHash := startLinuxE2EWebTransportEcho(t)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file, _ := json.Marshal(secret)
		page := strings.Replace(linuxE2EPage, "__FILE__", string(file), -1)
		page = strings.Replace(page, "__WT_URL__", wtURL, -1)
		page = strings.Replace(page, "__WT_HASH__", wtHash, -1)
		page = strings.Replace(page, "__FOREIGN__", foreign.URL, -1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL

	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	if supported, reason := agent.nativeAvailable(); !supported {
		t.Fatalf("native apps unavailable: %s", reason)
	}
	agent.startNativeHost()
	defer agent.stopNativeHost()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/probe", "capabilities": []any{"fs.readText", "externalOpen.files"}, "launch": false})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	app, _ := agent.nativeApp("e2e/probe")
	defer func() { _ = exec.Command("pkill", "-f", "--", "--id e2e/probe").Run() }()
	t.Logf("installed %v", result)
	entry, _ := os.ReadFile(app.Path)
	t.Logf("desktop entry:\n%s", entry)

	launch := func(files ...string) {
		t.Helper()
		// The host inherits gio's output; a pipe would stay open as long as
		// the app runs, so write to a file.
		output, err := os.OpenFile(filepath.Join(stateDir, "launch.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		command := exec.Command("gio", append([]string{"launch", app.Path}, files...)...)
		command.Env = append(os.Environ(), "DYNAPP_HOST_DEBUG=1")
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			log, _ := os.ReadFile(filepath.Join(stateDir, "launch.log"))
			t.Fatalf("gio launch %v: %v %s", files, err, log)
		}
	}
	// Cold launch from the launcher entry, with a file delivered before the
	// bridge loads.
	launch(secret)

	sawReport := map[string]bool{}
	sawOpen := false
	var webTransport map[string]any
	for _, wantReloaded := range []bool{false, true} {
		for {
			var report map[string]any
			select {
			case report = <-reports:
			case <-ctx.Done():
				t.Fatalf("the installed app never reported back (reloaded=%v); host log:\n%s", wantReloaded, hostLog(app))
			}
			if record, ok := report["opened"].(map[string]any); ok {
				if actual, _ := record["path"].(string); sameLinuxFile(actual, secret) {
					sawOpen = true
				}
				continue
			}
			if report["loaded"] != nil || report["key"] != nil {
				continue
			}
			if result, ok := report["webtransport"].(map[string]any); ok {
				if webTransport == nil {
					webTransport = result
				}
				continue
			}
			t.Logf("page report: %v", report)
			if report["ok"] != true {
				t.Fatalf("page failed: %v\nhost log:\n%s", report["error"], hostLog(app))
			}
			if report["reloaded"] != wantReloaded {
				t.Fatalf("report reloaded = %v, want %v", report["reloaded"], wantReloaded)
			}
			if report["platform"] != "linux" || report["storeId"] != "e2e/probe" {
				t.Fatalf("bridge identity = %v", report)
			}
			if fmt.Sprint(report["capabilities"]) != "[externalOpen.files fs.readText]" || report["pong"] != "pong" {
				t.Fatalf("capabilities or ping = %v", report)
			}
			read, _ := report["read"].(map[string]any)
			if read["type"] != "fs-result" || !strings.Contains(fmt.Sprint(read["result"]), "hello from the agent") {
				t.Fatalf("fs.readText through the bridge = %v", read)
			}
			denied, _ := report["exec"].(map[string]any)
			if !strings.Contains(fmt.Sprint(denied["error"]), "fs.exec is not granted") {
				t.Fatalf("ungranted exec = %v", denied)
			}
			sawReport[fmt.Sprint(wantReloaded)] = true
			break
		}
	}

	for webTransport == nil {
		select {
		case report := <-reports:
			if result, ok := report["webtransport"].(map[string]any); ok {
				webTransport = result
			}
		case <-ctx.Done():
			t.Fatalf("the page never reported its WebTransport session; host log:\n%s", hostLog(app))
		}
	}
	if webTransport["ok"] != true {
		t.Fatalf("WebTransport through the host = %v\nhost log:\n%s", webTransport, hostLog(app))
	}
	t.Logf("WebTransport through the host echoed %v bytes", webTransport["received"])

	waitOpen := func(path string) {
		t.Helper()
		if sawOpen && path == secret {
			return
		}
		for {
			select {
			case report := <-reports:
				record, _ := report["opened"].(map[string]any)
				if actual, _ := record["path"].(string); sameLinuxFile(actual, path) && record["base64"] != "" {
					t.Logf("file open delivered: %s", path)
					return
				}
				if report["stayed"] != nil {
					reports <- report
					time.Sleep(50 * time.Millisecond)
				}
			case <-ctx.Done():
				t.Fatalf("file %s never reached the page; host log:\n%s", path, hostLog(app))
			}
		}
	}
	waitOpen(secret)

	// A second launch with a file goes to the running window (GApplication).
	warm := filepath.Join(stateDir, "לידור warm.txt")
	if err := os.WriteFile(warm, []byte("warm file bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	launch(warm)
	waitOpen(warm)

	// The page's script navigation away from the app is stopped and the
	// cross-origin iframe loaded without the bridge.
	deadline := time.After(20 * time.Second)
	for stayed := false; !stayed; {
		select {
		case report := <-reports:
			if value, ok := report["stayed"]; ok {
				if value != origin {
					t.Fatalf("the app window left its origin: %v", value)
				}
				stayed = true
			}
		case <-deadline:
			t.Fatalf("no stay report; host log:\n%s", hostLog(app))
		}
	}
	frameLoaded := false
	for drained := false; !drained; {
		select {
		case event := <-foreignEvents:
			if strings.HasPrefix(event, "/frame-loaded") {
				frameLoaded = true
				if !strings.Contains(event, "host=undefined") {
					t.Fatalf("the iframe saw the native bridge: %s", event)
				}
				t.Logf("iframe: %s", event)
			}
		default:
			drained = true
		}
	}
	if !frameLoaded {
		t.Fatalf("the cross-origin iframe never loaded; host log:\n%s", hostLog(app))
	}
	if log := hostLog(app); !strings.Contains(log, "bridge: dropped a message") {
		t.Fatalf("the iframe's forged bridge message was not dropped:\n%s", log)
	}

	// An agent restart drops the control channel; the running app must
	// reconnect so later file opens still reach the agent.
	agent.stopNativeHost()
	agent.startNativeHost()
	restarted := filepath.Join(stateDir, "after restart.txt")
	if err := os.WriteFile(restarted, []byte("after restart"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	launch(restarted)
	for delivered := false; !delivered; {
		records, err := agent.handleExternalOpenRPC(ctx, message{Service: "externalOpen", Method: "takeData", AppID: "e2e/probe"})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records.([]map[string]any) {
			delivered = delivered || sameLinuxFile(record["path"].(string), restarted)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("file open after an agent restart never reached the agent; host log:\n%s", hostLog(app))
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("file open delivered after agent restart")

	// Keys reach the page before the host's accelerators: a page that
	// handles F5 keeps running, and an unhandled Ctrl+R still reloads.
	drainReports := func() {
		for {
			select {
			case <-reports:
			default:
				return
			}
		}
	}
	waitReport := func(match func(map[string]any) bool, within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case report := <-reports:
				if match(report) {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	output, _ := exec.Command("xdotool", "search", "--name", "^Native Probe").Output()
	windows := strings.Fields(string(output))
	if len(windows) == 0 {
		t.Fatalf("no app window to send keys to; host log:\n%s", hostLog(app))
	}
	window := windows[len(windows)-1]
	_ = exec.Command("xdotool", "windowactivate", "--sync", window).Run()
	time.Sleep(500 * time.Millisecond)
	drainReports()
	// xdotool key name -> the page's combo; Alt+F4 is the window manager's.
	keys := [][2]string{{"Escape", "Escape"}, {"Alt_L", "alt+Alt"}}
	// Alt+letter opens Commander's quick search.
	for _, c := range "abcdefghijklmnopqrstuvwxyz0123456789" {
		keys = append(keys, [2]string{"alt+" + string(c), "alt+" + string(c)})
	}
	for n := 1; n <= 12; n++ {
		f := fmt.Sprintf("F%d", n)
		keys = append(keys, [2]string{f, f}, [2]string{"shift+" + f, "shift+" + f}, [2]string{"ctrl+" + f, "ctrl+" + f})
		if n != 4 {
			keys = append(keys, [2]string{"alt+" + f, "alt+" + f})
		}
	}
	for _, key := range keys {
		if err := exec.Command("xdotool", "key", "--clearmodifiers", key[0]).Run(); err != nil {
			t.Fatalf("xdotool key %s: %v", key[0], err)
		}
		want := key[1]
		if !waitReport(func(r map[string]any) bool {
			if r["loaded"] == true {
				t.Fatalf("%s reloaded the page although the page handled it", want)
			}
			return r["key"] == want
		}, 5*time.Second) {
			t.Fatalf("the page never saw %s; host log:\n%s", want, hostLog(app))
		}
	}
	if waitReport(func(r map[string]any) bool { return r["loaded"] == true }, 2*time.Second) {
		t.Fatalf("a page-handled key reloaded the page")
	}
	if err := exec.Command("xdotool", "key", "--clearmodifiers", "ctrl+r").Run(); err != nil {
		t.Fatalf("xdotool key ctrl+r: %v", err)
	}
	if !waitReport(func(r map[string]any) bool { return r["loaded"] == true }, 10*time.Second) {
		t.Fatalf("an unhandled Ctrl+R did not reload the page; host log:\n%s", hostLog(app))
	}
	t.Logf("the page received %d keys (F1-F12 with Shift/Ctrl/Alt, Esc, Alt, Alt+letters/digits) without a reload; unhandled Ctrl+R reloaded it", len(keys))

	// Only the app host binary may use the app's channel.
	conn, err := net.Dial("unix", agent.nativeEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	impostor := newNativeFrameSocket(conn)
	writeNative(t, impostor, map[string]any{"type": "native-host-hello", "version": 1, "storeId": "e2e/probe", "purpose": "app"})
	if frame := readNative(t, impostor); frame["type"] != "native-host-error" {
		t.Fatalf("a foreign process was accepted: %v", frame)
	}
}

// TestNativeLinuxPermissionPrompt launches an app with no decision so the
// pairing prompt appears, screenshots it to $DYNAPP_SHEET_SCREENSHOT, then
// clicks Allow.
func TestNativeLinuxPermissionPrompt(t *testing.T) {
	host, shot := os.Getenv("DYNAPP_APP_HOST"), os.Getenv("DYNAPP_SHEET_SCREENSHOT")
	if host == "" || shot == "" {
		t.Skip("set DYNAPP_APP_HOST and DYNAPP_SHEET_SCREENSHOT")
	}
	t.Setenv("DYNAPP_NATIVE_APPS", "1")
	stateDir, err := os.MkdirTemp("/tmp", "dnh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	home := filepath.Join(stateDir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	opened := make(chan string, 4)
	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/apps/e2e/sheet", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"app": map[string]any{"name": "Sheet Probe"}, "hostedOrigins": []string{origin},
			"latestRevision": map[string]any{"manifest": map[string]any{"name": "Sheet Probe", "backendPermissions": []any{
				map[string]any{"permission": "fs.readText", "reason": "Opens the spreadsheets you pick so you can edit them here."},
				map[string]any{"permission": "fs.exec", "reason": "Runs the converter tool that turns legacy files into the current format."},
				map[string]any{"permission": "secrets.manage", "reason": "Stores the API key you enter so syncing keeps working."},
			}}},
		})
	})
	icon := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(linuxTestPNG(t, color.RGBA{120, 60, 200, 255}))
	}
	mux.HandleFunc("/icon-512.png", icon)
	mux.HandleFunc("/icon-512-maskable.png", icon)
	mux.HandleFunc("/opened", func(w http.ResponseWriter, r *http.Request) {
		select {
		case opened <- r.URL.Query().Get("caps"):
		default:
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><body style="font:20px system-ui;padding:40px">Sheet probe<script>
const s = window.__DYNAPP_NATIVE_HOST__.openAgentSocket();
s.addEventListener("open", () => s.send(JSON.stringify({ type: "hello", protocol: 2 })));
s.addEventListener("message", (event) => { const frame = JSON.parse(event.data); if (frame.type === "hello") fetch("/opened?caps=" + frame.capabilities.join(",")); });
</script>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL
	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	agent.startNativeHost()
	defer agent.stopNativeHost()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/sheet", "capabilities": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.revokeNativeGrant("e2e/sheet"); err != nil {
		t.Fatal(err)
	}
	app, _ := agent.nativeApp("e2e/sheet")
	defer func() { _ = exec.Command("pkill", "-f", "--", "--id e2e/sheet").Run() }()
	if err := nativeLaunch(app, agent.nativeEndpoint()); err != nil {
		t.Fatal(err)
	}
	// Wait for the prompt window, screenshot the screen, then press Allow:
	// the default button is Don't Allow, so Tab to Allow after the page's
	// one-second guard.
	var window string
	for deadline := time.Now().Add(20 * time.Second); window == "" && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		output, _ := exec.Command("xdotool", "search", "--name", "^Permissions$").Output()
		window = strings.TrimSpace(strings.Split(string(output), "\n")[0])
	}
	if window == "" {
		t.Fatalf("the permission prompt did not appear; host log:\n%s", hostLog(app))
	}
	time.Sleep(1500 * time.Millisecond)
	if output, err := exec.Command("import", "-window", "root", filepath.Join(shot, "permission-prompt.png")).CombinedOutput(); err != nil {
		t.Logf("screenshot: %v %s", err, output)
	}
	geometry, _ := exec.Command("xdotool", "getwindowgeometry", window).Output()
	t.Logf("prompt window %s: %s", window, strings.TrimSpace(string(geometry)))
	_ = exec.Command("xdotool", "windowactivate", "--sync", window).Run()
	_ = exec.Command("xdotool", "key", "--window", window, "Tab", "space").Run()
	select {
	case caps := <-opened:
		t.Logf("agent answered after the prompt with capabilities %q; host log:\n%s", caps, hostLog(app))
		if !strings.Contains(caps, "fs.readText") && !strings.Contains(caps, "secrets.manage") {
			t.Logf("note: keyboard focus picked a different button; capabilities %q", caps)
		}
	case <-time.After(30 * time.Second):
		_ = exec.Command("import", "-window", "root", filepath.Join(shot, "after-click.png")).Run()
		t.Fatalf("the app never got its agent connection after the prompt; host log:\n%s", hostLog(app))
	}
}

func hostLog(app NativeApp) string {
	data, _ := xdgDataHome()
	owner, slug, _ := strings.Cut(app.StoreID, "/")
	content, err := os.ReadFile(filepath.Join(data, "dynapp", "native", owner, slug, "host.log"))
	if err != nil {
		return "(no host log: " + err.Error() + ")"
	}
	return string(content)
}

// startLinuxE2EWebTransportEcho serves WebTransport on this machine's first
// private IPv4 address (the host refuses loopback): each stream echoes what
// it reads. It returns the URL and the base64 certificate hash.
func startLinuxE2EWebTransportEcho(t *testing.T) (string, string) {
	t.Helper()
	var address net.IP
	addresses, _ := net.InterfaceAddrs()
	for _, candidate := range addresses {
		if network, ok := candidate.(*net.IPNet); ok && network.IP.To4() != nil && network.IP.IsPrivate() {
			address = network.IP
			break
		}
	}
	if address == nil {
		t.Skip("no private IPv4 address for the WebTransport echo server")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lan"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{address}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: address})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server := &webtransport.Server{
		H3:          &http3.Server{TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}), Handler: mux},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(server.H3)
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		session, err := server.Upgrade(w, r)
		if err != nil {
			return
		}
		go func() {
			for {
				stream, err := session.AcceptStream(context.Background())
				if err != nil {
					return
				}
				go func() {
					data, _ := io.ReadAll(stream)
					_, _ = stream.Write(data)
					_ = stream.Close()
				}()
			}
		}()
	})
	go func() { _ = server.Serve(packet) }()
	t.Cleanup(func() { _ = server.Close() })
	return fmt.Sprintf("https://%s:%d/echo", address, packet.LocalAddr().(*net.UDPAddr).Port), base64.StdEncoding.EncodeToString(sum[:])
}
