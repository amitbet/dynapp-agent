package shellagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A native app's question about another machine is asked in its own window;
// the agent reads the request from Dyner and records the answer there.
func TestNativeRemotePermissionAsksInTheAppWindowAndDecidesInDyner(t *testing.T) {
	app := notesNativeApp()
	app.ApprovedAt = time.Now().UnixMilli()
	app.Capabilities = []string{"fs.readText"}
	app.Declared = []string{"fs.readText"}
	app.Denied = []string{"fs.exec", "net.protocol.https"}
	agent, page, control := startNativePageAndControl(t, app)

	var mu sync.Mutex
	var decided map[string]any
	var auth, readOrigin string
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/remote-environments/env_mac/permission-requests/rpr_0123456789" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		if r.Method == http.MethodGet {
			readOrigin = r.URL.Query().Get("origin")
			_ = json.NewEncoder(w).Encode(map[string]any{"request": map[string]any{
				"id": "rpr_0123456789", "status": "pending", "origin": app.Origin,
				"capabilities": []string{"servers.connect"},
				"environment":  map[string]any{"id": "env_desk", "name": "Desk"},
			}})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&decided)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "approved", "granted": []string{"servers.connect"}})
	}))
	t.Cleanup(dyner.Close)
	agent.mu.Lock()
	agent.Config.DynerBaseURL = dyner.URL
	agent.Config.EnvironmentID = "env_mac"
	agent.Config.DeviceCredential = "env_mac." + strings.Repeat("s", 43)
	agent.mu.Unlock()
	agent.DynerHTTPClient = dyner.Client()

	writeNative(t, page, map[string]any{"type": "rpc", "id": "r1", "service": "permissionRequest", "method": "remote", "args": []any{map[string]any{"requestId": "rpr_0123456789"}}})
	request := readNative(t, control)
	prompt, _ := request["page"].(string)
	if request["type"] != "native-host-permission-request" || !strings.Contains(prompt, `"machine":"Desk"`) || !strings.Contains(prompt, "servers.connect") {
		t.Fatalf("remote prompt = %#v", request)
	}
	writeNative(t, control, map[string]any{"type": "native-host-permission-decision", "requestId": request["requestId"], "decision": "allow", "capabilities": []string{"servers.connect", "fs.exec"}})
	result, _ := readNativeUntil(t, page, func(frame map[string]any) bool { return frame["id"] == "r1" })
	value, _ := result["result"].(map[string]any)
	if result["type"] != "rpc-result" || value["status"] != "approved" {
		t.Fatalf("remote result = %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "DynApp-Device env_mac."+strings.Repeat("s", 43) || readOrigin != app.Origin {
		t.Fatalf("Dyner saw auth %q origin %q", auth, readOrigin)
	}
	if decided["decision"] != "allow" || decided["origin"] != app.Origin {
		t.Fatalf("decision = %#v", decided)
	}
	if granted, _ := decided["capabilities"].([]any); len(granted) != 1 || granted[0] != "servers.connect" {
		t.Fatalf("only the asked permission is sent: %#v", decided["capabilities"])
	}
	// This machine's own grant is untouched.
	if stored, _ := agent.nativeApp(app.StoreID); stringSliceHas(stored.Capabilities, "servers.connect") {
		t.Fatalf("remote decision leaked into the local grant: %#v", stored)
	}
}

func TestRemotePermissionRPCIsForNativeAppsOnly(t *testing.T) {
	agent := &Server{}
	if _, err := agent.handleRemotePermissionRPC(t.Context(), message{auth: &socketAuthentication{storeID: "amit-bet/notes"}}); err == nil {
		t.Fatal("a browser socket must not raise a native prompt")
	}
}
