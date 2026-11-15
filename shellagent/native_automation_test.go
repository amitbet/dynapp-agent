package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// automationHost connects a fake host control channel that advertises
// automation (or not) and returns it after the ready frame.
func automationHost(t *testing.T, agent *Server, storeID string, features []string) (*nativeFrameSocket, map[string]any) {
	t.Helper()
	host := dialNativeHello(t, agent, map[string]any{"type": "native-host-hello", "version": 1, "storeId": storeID, "purpose": "control", "features": features})
	ready := readNative(t, host)
	if ready["type"] != "native-host-ready" {
		t.Fatalf("ready = %v", ready)
	}
	deadline := time.Now().Add(2 * time.Second)
	for agent.automationControl(storeID) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return host, ready
}

func dialNativeHello(t *testing.T, agent *Server, hello map[string]any) *nativeFrameSocket {
	t.Helper()
	hostSide, agentSide := net.Pipe()
	go agent.serveNativeConnection(agentSide)
	host := newNativeFrameSocket(hostSide)
	t.Cleanup(func() { _ = hostSide.Close() })
	writeNative(t, host, hello)
	return host
}

// answerHost replies to the next automation frames with fixed values.
func answerHost(t *testing.T, host *nativeFrameSocket, answer func(frame map[string]any) (any, string)) chan map[string]any {
	t.Helper()
	seen := make(chan map[string]any, 16)
	go func() {
		for {
			_ = host.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, data, err := host.Read(context.Background())
			if err != nil {
				close(seen)
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) != nil {
				continue
			}
			seen <- frame
			if frame["type"] != "native-host-automation" {
				continue
			}
			value, failure := answer(frame)
			reply := map[string]any{"type": "native-host-automation-result", "id": frame["id"]}
			if failure != "" {
				reply["error"] = failure
			} else {
				reply["value"] = value
			}
			_ = host.Write(context.Background(), 1, mustJSON(reply))
		}
	}()
	return seen
}

func TestAutomationNeedsTheUsersConsentInTheAppWindow(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	// Config saves happen after the in-memory change; keep them off disk so
	// the test's temporary directory can be removed while one is in flight.
	agent.StateDir = ""
	host, ready := automationHost(t, agent, "amit-bet/notes", []string{"automation"})
	if ready["automation"] != false || !strings.Contains(ready["bootstrapScript"].(string), `"automation":false`) {
		t.Fatalf("ready frame = %v", ready["automation"])
	}
	ctx := context.Background()
	if _, err := agent.runAutomation(ctx, automationRequest{Command: "eval", App: "notes", Code: "document.title"}, nil); err == nil || !strings.Contains(err.Error(), "automation is off") {
		t.Fatalf("eval before consent: %v", err)
	}
	approve := false
	frames := answerHost(t, host, func(frame map[string]any) (any, string) {
		switch frame["op"] {
		case "consent":
			return map[string]any{"approved": approve}, ""
		case "eval":
			if !strings.Contains(frame["script"].(string), "return (\ndocument.title\n);") || frame["fallback"] == nil {
				return nil, "unexpected script"
			}
			return `{"ok":true,"undefined":false,"value":"Notes"}`, ""
		}
		return nil, "unexpected op"
	})
	if _, err := agent.runAutomation(ctx, automationRequest{Command: "enable", App: "notes"}, nil); err == nil || !strings.Contains(err.Error(), "did not allow") {
		t.Fatalf("declined consent: %v", err)
	}
	if app, _ := agent.nativeApp("amit-bet/notes"); app.Automation {
		t.Fatal("a declined request enabled automation")
	}
	approve = true
	if _, err := agent.runAutomation(ctx, automationRequest{Command: "enable", App: "amit-bet/notes"}, nil); err != nil {
		t.Fatal(err)
	}
	if app, _ := agent.nativeApp("amit-bet/notes"); !app.Automation {
		t.Fatal("consent was not stored")
	}
	waitFrame(t, frames, func(frame map[string]any) bool {
		return frame["type"] == "native-host-automation-state" && frame["enabled"] == true
	})
	result, err := agent.runAutomation(ctx, automationRequest{Command: "eval", App: "Notes", Code: "document.title"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value := string(result.(map[string]any)["value"].(json.RawMessage)); value != `"Notes"` {
		t.Fatalf("eval value = %s", value)
	}

	// Console batches are kept only while automation is on.
	writeNative(t, host, map[string]any{"type": "native-host-console", "entries": []any{
		map[string]any{"level": "warn", "text": "careful", "time": 5},
		map[string]any{"level": "error", "source": "exception", "text": "boom"},
	}})
	var console map[string]any
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		reply, err := agent.runAutomation(ctx, automationRequest{Command: "console", App: "notes"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		console = reply.(map[string]any)
		if len(console["entries"].([]automationConsoleEntry)) == 2 {
			break
		}
	}
	entries := console["entries"].([]automationConsoleEntry)
	if len(entries) != 2 || entries[0].Text != "careful" || entries[1].Source != "exception" || console["cursor"] != uint64(2) {
		t.Fatalf("console = %v", console)
	}
	newer, _ := agent.runAutomation(ctx, automationRequest{Command: "console", App: "notes", Since: 1}, nil)
	if got := newer.(map[string]any)["entries"].([]automationConsoleEntry); len(got) != 1 || got[0].Text != "boom" {
		t.Fatalf("console since 1 = %v", got)
	}

	// "Stop Automation" in the app's menu turns it off for every client.
	writeNative(t, host, map[string]any{"type": "native-host-automation-stop"})
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if app, _ := agent.nativeApp("amit-bet/notes"); !app.Automation {
			return
		}
	}
	t.Fatal("the app's Stop Automation did not disable automation")
}

func waitFrame(t *testing.T, frames chan map[string]any, match func(map[string]any) bool) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("host connection closed")
			}
			if match(frame) {
				return
			}
		case <-timeout:
			t.Fatal("expected frame never arrived")
		}
	}
}

func TestAutomationRefusesHostsThatPredateIt(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	_, ready := automationHost(t, agent, "amit-bet/notes", nil)
	if _, present := ready["automation"]; present {
		t.Fatal("an old host was told about automation")
	}
	_, err := agent.runAutomation(context.Background(), automationRequest{Command: "enable", App: "notes"}, nil)
	if err == nil || !strings.Contains(err.Error(), "install it again") {
		t.Fatalf("enable on an old host: %v", err)
	}
	apps := agent.automationApps()
	if len(apps) != 1 || apps[0]["running"] != true || apps[0]["automationSupported"] != false {
		t.Fatalf("apps = %v", apps)
	}
}

func TestAutomationEnabledAppStartsWithConsoleCapture(t *testing.T) {
	app := notesNativeApp()
	app.Automation = true
	agent, _ := nativeTestAgent(t, app)
	_, ready := automationHost(t, agent, app.StoreID, []string{"automation"})
	if ready["automation"] != true || !strings.Contains(ready["bootstrapScript"].(string), `"automation":true`) {
		t.Fatalf("ready = %v", ready["automation"])
	}
}

func TestAutomationBodiesTryExpressionsFirst(t *testing.T) {
	for _, test := range []struct{ code, first, fallback string }{
		{"document.title", "return (\ndocument.title\n);", "document.title"},
		{"  a(); b();  ", "return (\na(); b()\n);", "a(); b();"},
		{"const x = 1; return x", "const x = 1; return x", "const x = 1; return x"},
		{"return 5", "return 5", "return 5"},
	} {
		first, fallback := automationBodies(test.code)
		if first != test.first || fallback != test.fallback {
			t.Errorf("automationBodies(%q) = %q, %q", test.code, first, fallback)
		}
	}
	script := automationScript("e1", "return 1;")
	if !strings.Contains(script, `.add("e1")`) || !strings.Contains(script, "__dynappDescribe") {
		t.Fatalf("script = %s", script)
	}
}

func TestAutomationNavigationStaysInTheApp(t *testing.T) {
	app := NativeApp{Origin: "https://amitbet-notes.dynapp.io", URL: "https://amitbet-notes.dynapp.io/?launch=pwa"}
	for raw, want := range map[string]string{
		"/#/settings": "https://amitbet-notes.dynapp.io/#/settings",
		"#/today":     "https://amitbet-notes.dynapp.io/?launch=pwa#/today",
		"https://amitbet-notes.dynapp.io:443/a?b=1": "https://amitbet-notes.dynapp.io:443/a?b=1",
	} {
		got, err := automationNavigationURL(app, raw)
		if err != nil || got != want {
			t.Errorf("navigate %q = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"https://evil.example/", "//evil.example/x", "javascript:alert(1)", ""} {
		if _, err := automationNavigationURL(app, raw); err == nil {
			t.Errorf("navigate %q was allowed", raw)
		}
	}
}

func TestAutomationResolvesAppsBySlugAndName(t *testing.T) {
	agent := &Server{Config: Config{NativeApps: NativeAppList{
		{StoreID: "amit-bet/notes", Name: "Notes"},
		{StoreID: "someone/notes", Name: "Other Notes"},
		{StoreID: "amit-bet/fastlane", Name: "Fastlane"},
	}}}
	if app, err := agent.resolveAutomationApp("fastlane"); err != nil || app.StoreID != "amit-bet/fastlane" {
		t.Fatalf("slug: %v %v", app, err)
	}
	if app, err := agent.resolveAutomationApp("Other Notes"); err != nil || app.StoreID != "someone/notes" {
		t.Fatalf("name: %v %v", app, err)
	}
	if _, err := agent.resolveAutomationApp("notes"); err == nil || !strings.Contains(err.Error(), "full store id") {
		t.Fatalf("ambiguous slug: %v", err)
	}
	if _, err := agent.resolveAutomationApp("missing"); err == nil {
		t.Fatal("unknown app resolved")
	}
}

func TestAutomationSocketServesTheCLI(t *testing.T) {
	if automationEndpoint("/tmp/x") == "" {
		t.Skip("automation is macOS and Windows only")
	}
	stateDir, err := os.MkdirTemp("/tmp", "dna-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	agent := &Server{StateDir: stateDir, Config: Config{NativeApps: NativeAppList{notesNativeApp()}}}
	agent.startAutomation()
	defer agent.stopAutomation()
	var stdout, stderr bytes.Buffer
	if code := RunNativeAutomationCLI([]string{"apps"}, stateDir, &stdout, &stderr); code != 0 {
		t.Fatalf("apps exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"storeId": "amit-bet/notes"`) || !strings.Contains(stdout.String(), `"running": false`) {
		t.Fatalf("apps output = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunNativeAutomationCLI([]string{"eval", "notes", "1+1"}, stateDir, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "automation is off") {
		t.Fatalf("eval without consent: %d %s", code, stderr.String())
	}

	// The MCP server lists the tools and reports errors as tool results.
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"eval","arguments":{"app":"notes","code":"1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"apps","arguments":{}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := (automationClient{stateDir: stateDir}).serveMCP(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("MCP replies = %v", lines)
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) || !strings.Contains(lines[1], `"name":"snapshot"`) {
		t.Fatalf("MCP initialize/list = %s / %s", lines[0], lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "automation is off") {
		t.Fatalf("MCP eval error = %s", lines[2])
	}
	if !strings.Contains(lines[3], "amit-bet/notes") {
		t.Fatalf("MCP apps = %s", lines[3])
	}
}

func TestAutomationCLIParsesFlagsAnywhere(t *testing.T) {
	options, positional, err := parseAutomationFlags([]string{"notes", "--x", "10", "--follow", "--width=300", "rest"})
	if err != nil || options["x"] != "10" || options["follow"] != "true" || options["width"] != "300" || strings.Join(positional, ",") != "notes,rest" {
		t.Fatalf("flags = %v %v %v", options, positional, err)
	}
	request, _, err := automationCLIRequest("key", map[string]string{"mods": "cmd+shift"}, []string{"notes", "a"})
	if err != nil || request.Key != "a" || strings.Join(normalizeModifiers(request.Modifiers), ",") != "meta,shift" {
		t.Fatalf("key request = %+v %v", request, err)
	}
}
