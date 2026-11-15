//go:build darwin && nativee2e

package shellagent

// End-to-end check of native app automation on macOS: a real installed app
// (Swift host + WKWebView) driven through the agent's automation socket.
//
//	go test -tags nativee2e -run TestNativeAutomation -v ./shellagent/

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const automationE2EPage = `<!doctype html><meta charset="utf-8"><title>automation e2e</title>
<style>body { margin: 0; font: 16px sans-serif } #go { position: absolute; left: 40px; top: 60px; width: 200px; height: 60px }
#field { position: absolute; left: 40px; top: 160px; width: 300px; height: 30px }</style>
<button id="go">Go</button><input id="field">
<script>
window.__probe = { clicks: [], keys: [], screen: "home", goTo(name) { this.screen = name; document.title = name; return name; } };
document.getElementById("go").addEventListener("click", (event) => { __probe.clicks.push({ trusted: event.isTrusted, detail: event.detail }); console.log("clicked", __probe.clicks.length); });
document.getElementById("field").addEventListener("keydown", (event) => __probe.keys.push({ key: event.key, trusted: event.isTrusted, meta: event.metaKey }));
console.warn("page ready", { n: 1 });
setTimeout(() => { throw new Error("late failure"); }, 50);
</script>`

func TestNativeAutomation(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "dna-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	t.Setenv("HOME", filepath.Join(stateDir, "home"))
	snapshots := filepath.Join(stateDir, "snapshots")
	_ = os.MkdirAll(snapshots, 0o700)

	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/apps/e2e/auto", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"app": map[string]any{"id": "e2e/auto", "name": "Automation Probe"}, "hostedOrigins": []string{origin},
			"latestRevision": map[string]any{"id": "rev_1", "manifest": map[string]any{"name": "Automation Probe"}},
		})
	})
	icon := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(testPNG(t, color.RGBA{200, 90, 30, 255})) }
	mux.HandleFunc("/icon-512.png", icon)
	mux.HandleFunc("/icon-512-maskable.png", icon)
	mux.HandleFunc("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"start_url": "/"}) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The hosted CSP forbids eval; automation must not depend on it.
		w.Header().Set("Content-Security-Policy", "script-src 'self' 'unsafe-inline'")
		_, _ = io.WriteString(w, automationE2EPage)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	origin = server.URL

	agent := &Server{StateDir: stateDir, Config: Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL}}
	agent.startNativeHost()
	defer agent.stopNativeHost()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := agent.installNativeApp(ctx, map[string]any{"storeId": "e2e/auto", "capabilities": []any{}, "launch": false}); err != nil {
		t.Fatalf("install: %v", err)
	}
	app, _ := agent.nativeApp("e2e/auto")
	defer func() {
		_ = exec.Command("/usr/bin/pkill", "-f", app.Executable).Run()
		_ = exec.Command(lsregisterPath, "-u", app.Path).Run()
	}()
	// Consent is asked in the window; this test grants it the way the sheet would.
	app.Automation = true
	agent.upsertNativeApp(app)
	if output, err := exec.Command("/usr/bin/open", "--env", "DYNAPP_HOST_SNAPSHOT="+snapshots, "-a", app.Path).CombinedOutput(); err != nil {
		t.Fatalf("open: %v %s", err, output)
	}
	for agent.automationControl("e2e/auto") == nil {
		select {
		case <-ctx.Done():
			t.Fatal("the app never connected its control channel")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !agent.automationControl("e2e/auto").supported {
		t.Fatal("the rebuilt host does not advertise automation")
	}

	client := automationClient{stateDir: stateDir}
	call := func(request automationRequest) json.RawMessage {
		t.Helper()
		request.App = "auto"
		result, err := client.call(request)
		if err != nil {
			t.Fatalf("%s: %v", request.Command, err)
		}
		return result
	}
	value := func(result json.RawMessage) string {
		t.Helper()
		var reply struct {
			Value     json.RawMessage `json:"value"`
			Undefined bool            `json:"undefined"`
		}
		_ = json.Unmarshal(result, &reply)
		if reply.Undefined {
			return "undefined"
		}
		return string(reply.Value)
	}
	eval := func(code string) string {
		t.Helper()
		return value(call(automationRequest{Command: "eval", Code: code}))
	}

	call(automationRequest{Command: "wait", Selector: "#go"})
	if got := eval("document.title"); got != `"automation e2e"` {
		t.Fatalf("title = %s", got)
	}
	if got := eval("const a = 2;\nreturn a * 21"); got != "42" {
		t.Fatalf("statements = %s", got)
	}
	if got := eval("await new Promise((resolve) => setTimeout(() => resolve('later'), 50))"); got != `"later"` {
		t.Fatalf("await = %s", got)
	}
	if got := eval("__probe.goTo('settings')"); got != `"settings"` {
		t.Fatalf("dev hook = %s", got)
	}
	if got := eval("document.querySelector('#go')"); !strings.Contains(got, `<button#go>`) {
		t.Fatalf("element = %s", got)
	}
	if got := eval("({ a: 1, self: window.__probe, big: 10n, nothing: undefined })"); !strings.Contains(got, `"a":1`) || !strings.Contains(got, `"big":"10n"`) {
		t.Fatalf("object = %s", got)
	}
	if _, err := client.call(automationRequest{Command: "eval", App: "auto", Code: "missingFunction()"}); err == nil || !strings.Contains(err.Error(), "ReferenceError") {
		t.Fatalf("thrown error = %v", err)
	}
	if got := eval("__probe.screen = 'x'; __probe.screen"); got != "undefined" {
		// Statements without return evaluate to undefined after the expression form fails to parse.
		t.Fatalf("statement fallback = %s", got)
	}

	// Console output and uncaught errors from page load onwards.
	var console struct {
		Entries []automationConsoleEntry `json:"entries"`
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		_ = json.Unmarshal(call(automationRequest{Command: "console"}), &console)
		joined := ""
		for _, entry := range console.Entries {
			joined += entry.Level + "/" + entry.Source + ":" + entry.Text + "\n"
		}
		if strings.Contains(joined, `warn/console:page ready {"n":1}`) && strings.Contains(joined, "error/exception:") && strings.Contains(joined, "late failure") && strings.Contains(joined, "info/navigation:Loaded") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console = %s", joined)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Screenshot: one image pixel per CSS pixel.
	var shot map[string]any
	_ = json.Unmarshal(call(automationRequest{Command: "snapshot"}), &shot)
	data, _ := base64.StdEncoding.DecodeString(shot["png"].(string))
	image, err := png.Decode(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("snapshot is not a PNG: %v", err)
	}
	if float64(image.Bounds().Dx()) != shot["viewportWidth"].(float64) {
		t.Fatalf("snapshot %dx%d, viewport %v", image.Bounds().Dx(), image.Bounds().Dy(), shot["viewportWidth"])
	}
	_ = os.WriteFile(filepath.Join(snapshots, "page.png"), data, 0o600)

	// Real input.
	call(automationRequest{Command: "click", Selector: "#go"})
	x, y := 140.0, 90.0
	call(automationRequest{Command: "click", X: &x, Y: &y, Count: 2})
	if got := eval("JSON.stringify(__probe.clicks)"); got != `"[{\"trusted\":true,\"detail\":1},{\"trusted\":true,\"detail\":1},{\"trusted\":true,\"detail\":2}]"` {
		t.Fatalf("clicks = %s", got)
	}
	call(automationRequest{Command: "type", Selector: "#field", Text: "Hi there"})
	call(automationRequest{Command: "key", Key: "Backspace"})
	if got := eval("document.querySelector('#field').value"); got != `"Hi ther"` {
		t.Fatalf("typed value = %s", got)
	}
	call(automationRequest{Command: "key", Key: "a", Modifiers: []string{"meta"}})
	if got := eval("__probe.keys.every((key) => key.trusted) && __probe.keys.some((key) => key.meta && key.key === 'a')"); got != "true" {
		t.Fatalf("keys = %s", eval("JSON.stringify(__probe.keys)"))
	}

	// Window geometry.
	width, height := 900.0, 600.0
	var window map[string]any
	_ = json.Unmarshal(call(automationRequest{Command: "window", Width: &width, Height: &height}), &window)
	if window["viewportWidth"] != 900.0 || window["viewportHeight"] != 600.0 || !strings.HasPrefix(window["url"].(string), origin) {
		t.Fatalf("window = %v", window)
	}

	// Navigation stays in the app; reload keeps automation working.
	if _, err := client.call(automationRequest{Command: "navigate", App: "auto", URL: "https://example.com/"}); err == nil {
		t.Fatal("navigation left the app")
	}
	call(automationRequest{Command: "navigate", URL: "/second"})
	call(automationRequest{Command: "wait", Expression: "location.pathname === '/second' && document.readyState === 'complete' && window.__probe"})
	call(automationRequest{Command: "reload"})
	time.Sleep(300 * time.Millisecond)
	call(automationRequest{Command: "wait", Selector: "#go"})
	if got := eval("__probe.clicks.length"); got != "0" {
		t.Fatalf("after reload = %s", got)
	}

	// Turning automation off stops commands; enable asks in the window.
	call(automationRequest{Command: "disable"})
	if _, err := client.call(automationRequest{Command: "eval", App: "auto", Code: "1"}); err == nil {
		t.Fatal("eval worked after disable")
	}
	go func() { _, _ = client.call(automationRequest{Command: "enable", App: "auto"}) }()
	sheet := filepath.Join(snapshots, "automation-consent.png")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if info, err := os.Stat(sheet); err == nil && info.Size() > 1000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the consent sheet was never shown")
		}
	}
	t.Logf("snapshots in %s", snapshots)
	if keep := os.Getenv("DYNAPP_AUTOMATION_E2E_KEEP"); keep != "" {
		_ = exec.Command("/bin/cp", "-R", snapshots, keep).Run()
	}
}
