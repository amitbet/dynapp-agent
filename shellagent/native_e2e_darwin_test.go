//go:build darwin && nativee2e

package shellagent

// End-to-end check of a real installed app on macOS: the agent builds and
// signs the bundle, LaunchServices starts the Swift host, the host loads a
// page through WKWebView, and the page reaches the agent over the bridge.
//
//	go test -tags nativee2e -run TestNativeAppEndToEnd -v ./shellagent/

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const e2ePage = `<!doctype html><meta charset="utf-8"><title>e2e</title><body>native e2e
<script>
const report = (data) => fetch("/report", { method: "POST", body: JSON.stringify(data) });
(async () => {
  const host = window.__DYNAPP_NATIVE_HOST__;
  if (!host) return report({ ok: false, error: "window.__DYNAPP_NATIVE_HOST__ is missing" });
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
  // The host keeps its socket table across page loads; the reloaded page
  // must still get its own agent connection.
  if (!reloaded) {
    sessionStorage.setItem("reloaded", "1");
    location.reload();
  }
})().catch((error) => report({ ok: false, error: String(error && error.message || error) }));
</script>`

func TestNativeAppEndToEnd(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "dnh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	home := filepath.Join(stateDir, "home")
	t.Setenv("HOME", home)
	secret := filepath.Join(stateDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("hello from the agent"), 0o600); err != nil {
		t.Fatal(err)
	}

	reports := make(chan map[string]any, 8)
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
					map[string]any{"permission": "externalOpen.files", "reason": "Open files from Finder."},
					map[string]any{"permission": "fs.readText", "reason": "Reads the probe file to prove the bridge works end to end."},
					map[string]any{"permission": "fs.exec", "reason": "Declared but not granted, to prove denials reach the page."},
				},
			}},
		})
	})
	icon := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testPNG(t, color.RGBA{30, 160, 90, 255}))
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file, _ := json.Marshal(secret)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, strings.Replace(e2ePage, "__FILE__", string(file), 1))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL

	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	agent.startNativeHost()
	defer agent.stopNativeHost()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/probe", "capabilities": []any{"fs.readText", "externalOpen.files"}, "launch": false})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	app, _ := agent.nativeApp("e2e/probe")
	defer func() {
		_ = exec.Command("/usr/bin/pkill", "-f", app.Executable).Run()
		_ = exec.Command(lsregisterPath, "-u", app.Path).Run()
	}()
	t.Logf("installed %v", result)
	// Finder-style cold launch, including a file delivered before the bridge loads.
	if output, err := exec.Command("/usr/bin/open", "-a", app.Path, secret).CombinedOutput(); err != nil {
		t.Fatalf("open cold: %v %s", err, output)
	}

	for _, wantReloaded := range []bool{false, true} {
		select {
		case report := <-reports:
			t.Logf("page report: %v", report)
			if report["ok"] != true {
				t.Fatalf("page failed: %v", report["error"])
			}
			if report["reloaded"] != wantReloaded {
				t.Fatalf("report reloaded = %v, want %v", report["reloaded"], wantReloaded)
			}
			if report["platform"] != "macos" || report["storeId"] != "e2e/probe" {
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
		case <-ctx.Done():
			t.Fatalf("the installed app never reported back (reloaded=%v)", wantReloaded)
		}
	}

	for _, path := range []string{secret, filepath.Join(stateDir, "לידור warm.txt")} {
		if path != secret {
			if err := os.WriteFile(path, []byte("warm file bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("/usr/bin/open", "-a", app.Path, path).CombinedOutput(); err != nil {
				t.Fatalf("open warm: %v %s", err, output)
			}
		}
		select {
		case report := <-reports:
			record, _ := report["opened"].(map[string]any)
			if actual, _ := record["path"].(string); !sameFile(actual, path) || record["base64"] == "" {
				t.Fatalf("Finder open was not delivered: %v", report)
			}
			t.Logf("Finder open delivered: %s", path)
		case <-ctx.Done():
			t.Fatal("Finder file never reached the page")
		}
	}

	// An agent restart (for example an update) drops the host's control
	// channel; the running app must reconnect so later Finder opens still
	// reach the agent.
	agent.stopNativeHost()
	agent.startNativeHost()
	restarted := filepath.Join(stateDir, "after restart.txt")
	if err := os.WriteFile(restarted, []byte("after restart"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/open", "-a", app.Path, restarted).CombinedOutput(); err != nil {
		t.Fatalf("open after restart: %v %s", err, output)
	}
	for delivered := false; !delivered; {
		records, err := agent.handleExternalOpenRPC(ctx, message{Service: "externalOpen", Method: "takeData", AppID: "e2e/probe"})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records.([]map[string]any) {
			delivered = delivered || sameFile(record["path"].(string), restarted)
		}
		select {
		case <-ctx.Done():
			t.Fatal("Finder open after an agent restart never reached the agent")
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("Finder open delivered after agent restart")

	// Only the installed host executable may use the app's channel.
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

// TestNativePermissionSheet launches an app with no decision so the pairing
// sheet appears, and saves a screenshot to $DYNAPP_SHEET_SCREENSHOT.
func TestNativePermissionSheet(t *testing.T) {
	shot := os.Getenv("DYNAPP_SHEET_SCREENSHOT")
	if shot == "" {
		t.Skip("set DYNAPP_SHEET_SCREENSHOT to an output directory")
	}
	stateDir, err := os.MkdirTemp("/tmp", "dnh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	t.Setenv("HOME", filepath.Join(stateDir, "home"))
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
	mux.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testPNG(t, color.RGBA{120, 60, 200, 255}))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><body style="font:20px system-ui;padding:40px">Sheet probe<script>const s = window.__DYNAPP_NATIVE_HOST__.openAgentSocket();</script>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL
	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	agent.startNativeHost()
	defer agent.stopNativeHost()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/sheet", "capabilities": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.revokeNativeGrant("e2e/sheet"); err != nil {
		t.Fatal(err)
	}
	app, _ := agent.nativeApp("e2e/sheet")
	defer func() {
		_ = exec.Command("/usr/bin/pkill", "-f", app.Executable).Run()
		_ = exec.Command(lsregisterPath, "-u", app.Path).Run()
	}()
	if output, err := exec.Command("/usr/bin/open", "--env", "DYNAPP_HOST_SNAPSHOT="+shot, app.Path).CombinedOutput(); err != nil {
		t.Fatalf("open: %v %s", err, output)
	}
	sheet := filepath.Join(shot, "permission-sheet.png")
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if info, err := os.Stat(sheet); err == nil && info.Size() > 0 {
			time.Sleep(500 * time.Millisecond)
			return
		}
	}
	t.Fatal("the permission sheet did not appear")
}

// TestNativeAppRunsThePWAShellRuntime loads the real PWA Shell runtime in an
// installed app. Point DYNAPP_PWA_SHELL_CONTENT at apps/pwa-shell/content in
// a dynapp checkout.
func TestNativeAppRunsThePWAShellRuntime(t *testing.T) {
	content := os.Getenv("DYNAPP_PWA_SHELL_CONTENT")
	if content == "" {
		t.Skip("set DYNAPP_PWA_SHELL_CONTENT to apps/pwa-shell/content")
	}
	stateDir, err := os.MkdirTemp("/tmp", "dnh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	t.Setenv("HOME", filepath.Join(stateDir, "home"))
	secret := filepath.Join(stateDir, "notes.txt")
	if err := os.WriteFile(secret, []byte("read through appShell.fs"), 0o600); err != nil {
		t.Fatal(err)
	}
	reports := make(chan map[string]any, 1)
	var origin string
	mux := http.NewServeMux()
	declared := []any{"fs.readText", "fs.writeText"}
	mux.HandleFunc("/api/v1/apps/e2e/runtime", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"app": map[string]any{"name": "Runtime Probe"}, "hostedOrigins": []string{origin},
			"latestRevision": map[string]any{"manifest": map[string]any{"name": "Runtime Probe", "backendPermissions": declared}},
		})
	})
	mux.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testPNG(t, color.RGBA{200, 140, 20, 255}))
	})
	mux.Handle("/runtime/", http.StripPrefix("/runtime/", http.FileServer(http.Dir(content))))
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		var report map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &report)
		select {
		case reports <- report:
		default:
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		context, _ := json.Marshal(map[string]any{
			"app": map[string]any{
				"id": "runtime", "storeId": "e2e/runtime", "name": "Runtime Probe", "version": "1",
				"backendPermissions": declared, "web": map[string]any{"ready": true},
				"pwa": map[string]any{"requiresRemoteEnvironment": false},
			},
			"shellVersion": "dyner-pwa", "pwaShellVersion": "dev", "apiOrigin": origin, "hostedOrigin": origin, "catalogHost": false,
		})
		file, _ := json.Marshal(secret)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="dynapp-web-context" content="%s">
<script>window.__errors = []; addEventListener("error", (event) => __errors.push(String(event.message) + " @" + event.filename + ":" + event.lineno));</script>
<script src="/runtime/appShell.js"></script><body>runtime probe<script>
const report = (data) => fetch("/report", { method: "POST", body: JSON.stringify(data) });
(async () => {
  const text = await window.appShell.fs.readText(%s);
  const info = await window.appShell.getInfo();
  let denied = "";
  try { await window.appShell.fs.writeText(%s + ".copy", "no"); } catch (error) { denied = String(error && error.message || error); }
  report({ ok: true, text, capabilities: info.capabilities, denied });
})().catch((error) => report({ ok: false, error: String(error && error.name) + ": " + String(error && error.message) + " | appShell=" + typeof window.appShell + " fs=" + typeof (window.appShell && window.appShell.fs) + " errors=" + JSON.stringify(window.__errors) }));
</script>`, strings.ReplaceAll(string(context), `"`, "&quot;"), file, file)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL
	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	agent.startNativeHost()
	defer agent.stopNativeHost()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/runtime", "capabilities": []any{"fs.readText"}, "launch": true}); err != nil {
		t.Fatal(err)
	}
	app, _ := agent.nativeApp("e2e/runtime")
	defer func() {
		_ = exec.Command("/usr/bin/pkill", "-f", app.Executable).Run()
		_ = exec.Command(lsregisterPath, "-u", app.Path).Run()
	}()
	select {
	case report := <-reports:
		t.Logf("runtime report: %v", report)
		if report["ok"] != true {
			t.Fatalf("runtime failed: %v", report["error"])
		}
		if report["text"] != "read through appShell.fs" && fmt.Sprint(report["text"]) == "" {
			t.Fatalf("readText = %v", report["text"])
		}
		denied := fmt.Sprint(report["denied"])
		if !strings.Contains(denied, "fs.writeText") || !strings.Contains(denied, "Permissions… in the app menu") {
			t.Fatalf("ungranted write = %q", denied)
		}
	case <-ctx.Done():
		t.Fatal("the runtime never reported back")
	}
}
