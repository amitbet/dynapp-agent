package shellagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestChromeResumesSessionAcrossCarrierLoss runs the real PWA Shell transport
// (createResumableSocket over openRemoteRecordSocket's WebRTC carrier) in
// headless Chrome against this agent, with a fake Dyner for signaling. The
// browser streams a process, drops its carrier mid-stream as a network change
// would, and must receive the whole output exactly once.
//
// Opt in: DYNAPP_E2E_CHROME=1, DYNAPP_REPO (default ../../dynapp) and a
// Playwright install reachable through PLAYWRIGHT_NODE_PATH; CHROME_PATH picks a
// local Chrome when Playwright's own browser build is missing.
func TestChromeResumesSessionAcrossCarrierLoss(t *testing.T) {
	if os.Getenv("DYNAPP_E2E_CHROME") != "1" {
		t.Skip("set DYNAPP_E2E_CHROME=1 to run the Chrome end-to-end test")
	}
	repo := os.Getenv("DYNAPP_REPO")
	if repo == "" {
		repo = filepath.Join("..", "..", "dynapp")
	}
	shellDir, err := filepath.Abs(filepath.Join(repo, "shared", "shell"))
	if err != nil {
		t.Fatal(err)
	}
	agent := resumeTestAgent("fs.execFile")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type fakeSession struct {
		Offer             any   `json:"offer"`
		Answer            any   `json:"answer"`
		BrowserCandidates []any `json:"browserCandidates"`
		ShellCandidates   []any `json:"shellCandidates"`
	}
	var mu sync.Mutex
	sessions := map[string]*fakeSession{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "content-type, authorization")
		if r.Method == http.MethodOptions {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/shell/") {
			http.ServeFile(w, r, filepath.Join(shellDir, strings.TrimPrefix(r.URL.Path, "/shell/")))
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("content-type", "text/html")
			_, _ = w.Write([]byte(e2eChromePage))
			return
		}
		const prefix = "/api/v1/remote-environments/env_test/ice/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		var body any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case rest[0] == "servers":
			_ = json.NewEncoder(w).Encode(map[string]any{"iceServers": []any{}, "expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case rest[0] == "sessions" && len(rest) == 1:
			raw := make([]byte, 16)
			_, _ = rand.Read(raw)
			id := "ice_" + base64.RawURLEncoding.EncodeToString(raw)[:22]
			sessions[id] = &fakeSession{}
			_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": id}})
		case rest[0] == "sessions" && len(rest) >= 2:
			session := sessions[rest[1]]
			if session == nil {
				http.NotFound(w, r)
				return
			}
			action := ""
			if len(rest) == 3 {
				action = rest[2]
			}
			switch action {
			case "offer":
				session.Offer = body
				encoded, _ := json.Marshal(body)
				var offer iceDescription
				_ = json.Unmarshal(encoded, &offer)
				id := rest[1]
				go func() {
					if err := agent.answerICESession(ctx, Config{DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret"}, iceSession{ID: id, Offer: &offer}); err != nil {
						t.Logf("answer %s: %v", id, err)
					}
				}()
			case "answer":
				session.Answer = body
			case "browser-candidates":
				session.BrowserCandidates, _ = body.([]any)
			case "shell-candidates":
				session.ShellCandidates, _ = body.([]any)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
				"id": rest[1], "offer": session.Offer, "answer": session.Answer,
				"browserCandidates": session.BrowserCandidates, "shellCandidates": session.ShellCandidates,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	script := filepath.Join(t.TempDir(), "drive.mjs")
	if err := os.WriteFile(script, []byte(e2eChromeDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "node", script, server.URL+"/")
	command.Env = append(os.Environ(), "NODE_PATH="+os.Getenv("PLAYWRIGHT_NODE_PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("driver failed: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var result struct {
		Stdout    string   `json:"stdout"`
		Starts    int      `json:"starts"`
		Exits     int      `json:"exits"`
		Events    []string `json:"events"`
		Resumable bool     `json:"resumable"`
		Carrier   string   `json:"carrier"`
		Error     string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("driver output: %v\n%s", err, output)
	}
	t.Logf("events=%v carrier=%s resumable=%v", result.Events, result.Carrier, result.Resumable)
	if result.Error != "" {
		t.Fatalf("scenario failed: %s\n%s", result.Error, output)
	}
	var want strings.Builder
	for index := 1; index <= e2eLines; index++ {
		want.WriteString("line")
		want.WriteString(strconv.Itoa(index))
		want.WriteString("\n")
	}
	if !result.Resumable || result.Starts != 1 || result.Exits != 1 || result.Stdout != want.String() {
		t.Fatalf("stdout after resume (starts=%d exits=%d resumable=%v):\n%q", result.Starts, result.Exits, result.Resumable, result.Stdout)
	}
	if !containsString(result.Events, "interrupted") || !containsString(result.Events, "resumed") {
		t.Fatalf("expected an interruption and a resume, got %v", result.Events)
	}
}

const e2eLines = 300

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

const e2eChromeDriver = `
import { createRequire } from "node:module";
const require = createRequire((process.env.NODE_PATH || process.cwd()) + "/");
const { chromium } = require("playwright");
const url = process.argv[2];
const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH || undefined, args: ["--disable-features=WebRtcHideLocalIpsWithMdns"] });
try {
  const page = await browser.newPage();
  page.on("console", (message) => console.log("[page] " + message.text()));
  await page.goto(url);
  await page.waitForFunction(() => typeof window.runScenario === "function");
  const result = await page.evaluate(() => window.runScenario());
  console.log(JSON.stringify(result));
} finally {
  await browser.close();
}
`

const e2eChromePage = `<!doctype html><meta charset="utf-8"><script type="module">
import { createResumableSocket, openCarrier } from "/shell/remoteRecordTransport.js";
const base = location.origin + "/api/v1/remote-environments/env_test/ice/sessions";
const post = async (url, body) => (await fetch(url, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body ?? {}) })).json();
const signaling = {
  create: () => post(base),
  offer: (id, offer) => post(base + "/" + id + "/offer", offer),
  candidates: (id, list) => post(base + "/" + id + "/browser-candidates", list),
  get: async (id) => (await fetch(base + "/" + id)).json(),
};
const hello = { type: "hello", protocol: 2, keyId: "browser-test", fragments: true };
const descriptor = { carrier: "webrtc", signaling, iceServers: [{ urls: "stun:127.0.0.1:9" }], hello, answerTimeoutMs: 15000, connectTimeoutMs: 8000 };
window.runScenario = async () => {
  const events = [];
  const dial = ({ offer }) => {
    let cancelled = false;
    const done = (async () => {
      const socket = await openCarrier(descriptor);
      if (cancelled) { socket.close(); return; }
      await offer({ socket, descriptor });
    })();
    return { done, cancel() { cancelled = true; } };
  };
  const socket = createResumableSocket({ dial, onEvent: (event) => { events.push(event.type); console.log("event " + event.type + " " + (event.reason || "")); } });
  let stdout = "", starts = 0, exits = 0, helloReply = null;
  let resolveExit, resolveHello;
  const exited = new Promise((resolve) => { resolveExit = resolve; });
  const helloDone = new Promise((resolve) => { resolveHello = resolve; });
  socket.addEventListener("open", () => socket.send(JSON.stringify(hello)));
  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    if (!helloReply) { helloReply = message; resolveHello(message); return; }
    if (message.type === "exec-start") starts += 1;
    if (message.type === "exec-output" && message.stream === "stdout") stdout += message.data;
    if (message.type === "exec-exit") { exits += 1; resolveExit(); }
  });
  let closed = null;
  socket.addEventListener("close", (event) => { closed = event; resolveExit(); });
  const timeout = (ms, what) => new Promise((_, reject) => setTimeout(() => reject(new Error(what + " timed out")), ms));
  try {
    await Promise.race([helloDone, timeout(20000, "hello")]);
    const script = "i=1; while [ $i -le 300 ]; do echo line$i; i=$((i+1)); sleep 0.01; done";
    socket.send(JSON.stringify({ type: "exec", id: "stream", stream: true, file: "/bin/sh", args: ["-c", script] }));
    const started = Date.now();
    while (!stdout.includes("line50\n")) {
      if (Date.now() - started > 15000) throw new Error("no output before the network change");
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    socket.reconnect("test network change");
    await Promise.race([exited, timeout(40000, "process exit")]);
    if (closed) throw new Error("session closed: " + closed.code + " " + closed.reason);
    return { stdout, starts, exits, events, resumable: socket.resumable, carrier: socket.carrier };
  } catch (error) {
    return { stdout, starts, exits, events, resumable: socket.resumable, carrier: socket.carrier, error: String(error && error.message || error) };
  } finally {
    socket.close();
  }
};
</script>`
