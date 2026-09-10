package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

type recordingProtocolSocket struct {
	testProtocolSocket
	mu       sync.Mutex
	messages []map[string]any
}

func (s *recordingProtocolSocket) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	s.mu.Lock()
	s.messages = append(s.messages, value)
	s.mu.Unlock()
	return nil
}

func (s *recordingProtocolSocket) events() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.messages...)
}

func TestClaudeSessionMatchesOfficeAgentContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX launcher")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	bin := t.TempDir()
	fixture := filepath.Join(bin, "claude-fixture.mjs")
	source := `import readline from "node:readline";if(process.argv.includes("--version")){console.log("claude 1.0");process.exit(0)}if(process.argv.includes("auth")&&process.argv.includes("status")){process.exit(0)}const lines=readline.createInterface({input:process.stdin});for await(const line of lines){const m=JSON.parse(line);if(m.type==="control_request"){process.stdout.write(JSON.stringify({type:"control_response",response:{subtype:"success",request_id:m.request_id,response:{models:[]}}})+"\n");continue;}if(m.type==="user"){process.stdout.write(JSON.stringify({type:"stream_event",event:{type:"content_block_delta",delta:{type:"text_delta",text:"draft"}}})+"\n");process.stdout.write(JSON.stringify({type:"result",subtype:"success",is_error:false})+"\n");}}`
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(bin, "claude")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+shellQuote(node)+" "+shellQuote(fixture)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DYNAPP_CLAUDE_BINARY", launcher)
	socket := &recordingProtocolSocket{}
	service := newAgentService(&Server{StateDir: t.TempDir()}, socket)
	defer service.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, err := service.handle(ctx, message{Method: "start", AppID: "gridbook", Args: []any{map[string]any{"agentId": "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := started.(map[string]any)["sessionId"].(string)
	models, err := service.handle(ctx, message{Method: "listModels", SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	first := models.([]map[string]any)[0]
	if first["id"] != "opus" || first["label"] != "Auto" || first["defaultEffort"] != "medium" {
		t.Fatalf("models = %#v", models)
	}
	if _, err := service.handle(ctx, message{Method: "setModel", SessionID: sessionID, Args: []any{map[string]any{"model": "claude-sonnet-5", "effort": "low"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.handle(ctx, message{Method: "prompt", SessionID: sessionID, Args: []any{map[string]any{"text": "Draft a title"}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		foundDelta, foundEnd := false, false
		for _, envelope := range socket.events() {
			event := objectValue(envelope["event"])
			foundDelta = foundDelta || event["type"] == "delta" && event["text"] == "draft"
			foundEnd = foundEnd || event["type"] == "turn-end" && event["status"] == "completed"
		}
		if foundDelta && foundEnd {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude events = %#v", socket.events())
}

func TestMCPProxyListsAndRoutesTools(t *testing.T) {
	token := "secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/tools":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "set_cells", "description": "Edit cells", "inputSchema": map[string]any{"type": "object"}}})
		case "/call":
			var call map[string]any
			_ = json.NewDecoder(r.Body).Decode(&call)
			if call["name"] != "set_cells" {
				t.Errorf("call = %#v", call)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "text": `{"changed":1}`})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set_cells","arguments":{"value":"42"}}}`,
	}, "\n") + "\n")
	var output bytes.Buffer
	if err := RunMCPProxy(server.URL, token, input, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("output = %q", output.String())
	}
	var listed, called map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &listed)
	_ = json.Unmarshal([]byte(lines[2]), &called)
	tools := objectValue(listed["result"])["tools"].([]any)
	if len(tools) != 1 || objectValue(tools[0])["name"] != "set_cells" {
		t.Fatalf("listed = %#v", listed)
	}
	result := objectValue(called["result"])
	if result["isError"] != false || !strings.Contains(stringValue(result["content"]), "changed") {
		t.Fatalf("called = %#v", called)
	}
}

func TestInstalledClaudeProviderHandshake(t *testing.T) {
	if os.Getenv("DYNAPP_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("set DYNAPP_TEST_LIVE_CLAUDE=1 for an installed authenticated CLI handshake")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is unavailable")
	}
	service := newAgentService(&Server{StateDir: t.TempDir()}, testProtocolSocket{})
	defer service.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	available := false
	for _, provider := range discoverAgents(ctx) {
		if provider["id"] == "claude" && provider["available"] == true {
			available = true
		}
	}
	if !available {
		t.Skip("installed Claude CLI is not authenticated")
	}
	started, err := service.handle(ctx, message{Method: "start", AppID: "handshake-test", Args: []any{map[string]any{"agentId": "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	if started.(map[string]any)["agentId"] != "claude" {
		t.Fatalf("started = %#v", started)
	}
}
