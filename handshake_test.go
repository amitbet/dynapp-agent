package shellagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func rawGet(t *testing.T, target string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(request)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response
}

func TestLoopbackWebSocketRejectsNonLoopbackHost(t *testing.T) {
	server := httptest.NewServer((&Server{StateDir: t.TempDir(), autoApprovePairings: true}).Handler())
	defer server.Close()
	response := rawGet(t, server.URL+RemotePath, func(r *http.Request) {
		r.Host = "agent.evil.example:9011"
		r.Header.Set("Origin", "http://localhost:5179")
	})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound Host status = %d, want 403", response.StatusCode)
	}
}

func TestLoopbackWebSocketRejectsDisallowedOrigin(t *testing.T) {
	server := httptest.NewServer((&Server{StateDir: t.TempDir(), autoApprovePairings: true}).Handler())
	defer server.Close()
	for _, origin := range []string{"https://evil.example", "http://dynapp.io", "https://dynapp.io.evil.example", ""} {
		response := rawGet(t, server.URL+RemotePath, func(r *http.Request) {
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		})
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q status = %d, want 403", origin, response.StatusCode)
		}
	}
}

func TestProductionHandlerRejectsHelloLocalWithoutChallenge(t *testing.T) {
	server := httptest.NewServer((&Server{StateDir: t.TempDir(), autoApprovePairings: true}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := http.Header{"Origin": []string{"http://localhost:5179"}}
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+RemotePath, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{"type": "hello", "protocol": ProtocolVersion, "local": true})
	_, _, err = connection.Read(ctx)
	if websocket.CloseStatus(err) != closeIdentityRejected {
		t.Fatalf("hello.local was not rejected: %v", err)
	}
}

// permissionTestDyner serves the catalog app record (and one hosted app) the
// way Dyner does, so authority identities resolve the catalog manifest.
func permissionTestDyner(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("app lookup must not carry credentials: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/apps/amit-bet/dyner":
			writeJSON(w, map[string]any{
				"app":            map[string]any{"id": "amit-bet/dyner", "name": "Dyner"},
				"latestRevision": map[string]any{"id": "rev_c", "manifest": map[string]any{"name": "Dyner", "backendPermissions": []any{"apps.manage", "remoteEnv.manage", "sessions.manage"}}},
			})
		case "/api/v1/apps/amit-bet/notes":
			writeJSON(w, map[string]any{
				"app":           map[string]any{"id": "amit-bet/notes", "name": "Notes"},
				"hostedOrigins": []string{"https://amitbet-notes.dynapp.io"},
				"latestRevision": map[string]any{
					"id": "rev_1",
					"manifest": map[string]any{
						"name": "Notes",
						"backendPermissions": []any{
							"fs.readText",
							map[string]any{"permission": "fs.exec", "request": "on-demand"},
							map[string]any{"permission": "net.protocol.https", "connect": []any{"https://api.example/v1/"}},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// newAuthorityAgent builds a production-handler agent whose Dyner base origin
// is the test Dyner server, plus an authority socket connected from it.
func newAuthorityAgent(t *testing.T, ctx context.Context) (*Server, *httptest.Server, *httptest.Server, *websocket.Conn) {
	t.Helper()
	dyner := permissionTestDyner(t)
	t.Cleanup(dyner.Close)
	agent := &Server{StateDir: t.TempDir(), Config: Config{DynerBaseURL: dyner.URL, AppDomain: "dynapp.io"}, DynerHTTPClient: dyner.Client()}
	server := httptest.NewServer(agent.Handler())
	t.Cleanup(server.Close)
	authority, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: dyner.URL, StoreID: "amit-bet/dyner", Declared: []string{"apps.manage"}, ReturnPending: true})
	t.Cleanup(func() { authority.Close(websocket.StatusNormalClosure, "") })
	if hello["type"] != "hello" {
		t.Fatalf("authority handshake frame = %#v (must not wait for a decision)", hello)
	}
	capabilities := anyStrings(hello["capabilities"])
	if !stringSliceHas(capabilities, "permissions.manage") || !stringSliceHas(capabilities, "apps.manage") || !stringSliceHas(capabilities, "remoteEnv.manage") || !stringSliceHas(capabilities, "sessions.manage") {
		t.Fatalf("authority capabilities = %#v", capabilities)
	}
	return agent, dyner, server, authority
}

func anyStrings(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func permissionsRPC(t *testing.T, ctx context.Context, authority *websocket.Conn, id, method string, args ...any) map[string]any {
	t.Helper()
	sendRequest(t, ctx, authority, map[string]any{"type": "rpc", "id": id, "service": "permissions", "method": method, "args": args})
	for {
		reply := receive(t, ctx, authority)
		if reply["type"] == "rpc-event" {
			continue
		}
		if reply["type"] != "rpc-result" || reply["id"] != id {
			t.Fatalf("permissions.%s reply = %#v", method, reply)
		}
		result, _ := reply["result"].(map[string]any)
		return result
	}
}

func TestAuthorityIdentityIsRecordedSilentlyAndPreapprovesPairings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent, dyner, server, authority := newAuthorityAgent(t, ctx)
	stored, err := LoadConfig(agent.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.BrowserIdentities) != 1 || !stored.BrowserIdentities[0].Authority || stored.BrowserIdentities[0].StoreID != "amit-bet/dyner" || stored.BrowserIdentities[0].Origin != strings.ToLower(dyner.URL) {
		t.Fatalf("authority identity = %#v", stored.BrowserIdentities)
	}
	browser := newTestBrowser(t)
	declared := []string{"fs.readText", "fs.writeText", "fs.exec", "net.protocol.https"}
	permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "owner/notes", "capabilities": []string{"fs.readText", "fs.exec", "secrets.manage"}})
	app, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: declared, Browser: browser, ReturnPending: true})
	defer app.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || hello["ok"] != true {
		t.Fatalf("hello after pre-approval = %#v", hello)
	}
	if capabilities := anyStrings(hello["capabilities"]); len(capabilities) != 2 || capabilities[0] != "fs.exec" || capabilities[1] != "fs.readText" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	stored, _ = LoadConfig(agent.StateDir)
	var appIdentity *BrowserIdentity
	for index := range stored.BrowserIdentities {
		if stored.BrowserIdentities[index].StoreID == "owner/notes" {
			appIdentity = &stored.BrowserIdentities[index]
		}
	}
	if appIdentity == nil || appIdentity.ApprovedAt == 0 || len(appIdentity.Capabilities) != 2 || len(appIdentity.Declared) != 4 || appIdentity.Authority {
		t.Fatalf("persisted identity = %#v", stored.BrowserIdentities)
	}
	if list := permissionsRPC(t, ctx, authority, "list2", "list"); len(list["pending"].([]any)) != 0 || len(list["grants"].([]any)) != 2 {
		t.Fatalf("list after approval = %#v", list)
	}
	// Reconnecting with the same key, origin, and app goes straight to hello
	// and the deliberately unchecked fs.writeText is not asked again.
	again, frame := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: declared, Browser: browser, ReturnPending: true})
	defer again.Close(websocket.StatusNormalClosure, "")
	if frame["type"] != "hello" {
		t.Fatalf("paired reconnect frame = %#v", frame)
	}
	// A newly declared permission stays denied on a direct reconnect. No
	// in-app approval request is created. The stored ceiling grows so Dyner
	// → Permissions can offer the new access.
	third, delta := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: append(declared, "clipboard.write"), Browser: browser, ReturnPending: true})
	defer third.Close(websocket.StatusNormalClosure, "")
	if delta["type"] != "hello" || stringSliceHas(anyStrings(delta["capabilities"]), "clipboard.write") {
		t.Fatalf("direct reconnect granted an unreviewed delta = %#v", delta)
	}
	third.Close(websocket.StatusNormalClosure, "")
	stored, _ = LoadConfig(agent.StateDir)
	appIdentity = nil
	for index := range stored.BrowserIdentities {
		if stored.BrowserIdentities[index].StoreID == "owner/notes" {
			appIdentity = &stored.BrowserIdentities[index]
		}
	}
	if appIdentity == nil || !stringSliceHas(appIdentity.Declared, "clipboard.write") || stringSliceHas(appIdentity.Capabilities, "clipboard.write") {
		t.Fatalf("delta reconnect did not raise the declared ceiling = %#v", stored.BrowserIdentities)
	}
	updated := permissionsRPC(t, ctx, authority, "grant-delta", "update", map[string]any{
		"keyId": appIdentity.KeyID, "origin": appIdentity.Origin, "storeId": appIdentity.StoreID,
		"capabilities": []string{"fs.readText", "clipboard.write"},
	})
	if got := anyStrings(updated["grant"].(map[string]any)["capabilities"]); !stringSliceHas(got, "clipboard.write") || !stringSliceHas(got, "fs.readText") {
		t.Fatalf("permissions update did not grant the new declaration = %#v", updated)
	}
	// A fresh Dyner launch replaces the existing grant, including newly
	// declared permissions. This is how Commander recovers an old pairing.
	permissionsRPC(t, ctx, authority, "pre2", "preapprove", map[string]any{"storeId": "owner/notes", "capabilities": []string{"fs.readText", "clipboard.write"}})
	fourth, refreshed := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: append(declared, "clipboard.write"), Browser: browser, ReturnPending: true})
	defer fourth.Close(websocket.StatusNormalClosure, "")
	if !reflect.DeepEqual(anyStrings(refreshed["capabilities"]), []string{"clipboard.write", "fs.readText"}) {
		t.Fatalf("fresh pre-approval did not replace the saved grant = %#v", refreshed)
	}
	// A second authority socket from the same browser key authenticates as
	// the recorded authority without a second record.
	authorityBrowserKey := stored.BrowserIdentities[0].KeyID
	stored, _ = LoadConfig(agent.StateDir)
	if count := len(stored.BrowserIdentities); count != 2 || stored.BrowserIdentities[0].KeyID != authorityBrowserKey {
		t.Fatalf("identities after flow = %#v", stored.BrowserIdentities)
	}
}

func TestUnknownPairingWaitsForDynerApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, server, authority := newAuthorityAgent(t, ctx)
	app, pending := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", ReturnPending: true})
	defer app.Close(websocket.StatusNormalClosure, "")
	if pending["type"] != "dynapp-direct-local-pending" || pending["reviewUrl"] == "" {
		t.Fatalf("pending = %#v", pending)
	}
	permissionsRPC(t, ctx, authority, "approve", "decide", map[string]any{"id": pending["requestId"], "capabilities": []string{}})
	if hello := receive(t, ctx, app); hello["type"] != "hello" || hello["ok"] != true {
		t.Fatalf("hello after approval = %#v", hello)
	}
}

func TestUnknownKeyInheritsApprovedOriginGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent, _, server, authority := newAuthorityAgent(t, ctx)
	declared := []string{"fs.readText", "fs.writeText", "fs.exec"}
	permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "owner/notes", "capabilities": []string{"fs.readText", "fs.exec"}})
	firstBrowser := newTestBrowser(t)
	first, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: declared, Browser: firstBrowser, ReturnPending: true})
	defer first.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || !reflect.DeepEqual(anyStrings(hello["capabilities"]), []string{"fs.exec", "fs.readText"}) {
		t.Fatalf("first hello = %#v", hello)
	}
	secondBrowser := newTestBrowser(t)
	second, inherited := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: declared, Browser: secondBrowser, ReturnPending: true})
	defer second.Close(websocket.StatusNormalClosure, "")
	if inherited["type"] != "hello" || !reflect.DeepEqual(anyStrings(inherited["capabilities"]), []string{"fs.exec", "fs.readText"}) {
		t.Fatalf("inherited hello = %#v", inherited)
	}
	if secondBrowser.keyID == firstBrowser.keyID {
		t.Fatal("test browsers used the same key")
	}
	stored, err := LoadConfig(agent.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, identity := range stored.BrowserIdentities {
		if identity.StoreID != "owner/notes" {
			continue
		}
		keys++
		if !reflect.DeepEqual(identity.Capabilities, []string{"fs.exec", "fs.readText"}) {
			t.Fatalf("inherited identity = %#v", identity)
		}
	}
	if keys != 2 {
		t.Fatalf("expected two owner/notes identities, got %#v", stored.BrowserIdentities)
	}
	other, pending := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/other", Declared: declared, ReturnPending: true})
	defer other.Close(websocket.StatusNormalClosure, "")
	if pending["type"] != "dynapp-direct-local-pending" {
		t.Fatalf("other app pending = %#v", pending)
	}
}

func TestHandshakePersistsAndListsBrowserClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent := newTestServer(t)
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	client := &BrowserClient{Browser: "Chrome", Version: "140", Platform: "macOS", Mode: "tab"}
	browser := newTestBrowser(t)
	connection, hello := dialTestAgent(t, ctx, server, testDialOptions{
		Origin: "http://localhost:5179", StoreID: "owner/notes", Browser: browser, Client: client,
	})
	defer connection.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" {
		t.Fatalf("hello = %#v", hello)
	}
	stored, err := LoadConfig(agent.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.BrowserIdentities) != 1 || stored.BrowserIdentities[0].Client.Label != "Chrome 140 on macOS · browser tab" {
		t.Fatalf("stored client = %#v", stored.BrowserIdentities)
	}
	updated := &BrowserClient{Browser: "Safari", Platform: "macOS", Mode: "pwa"}
	again, _ := dialTestAgent(t, ctx, server, testDialOptions{
		Origin: "http://localhost:5179", StoreID: "owner/notes", Browser: browser, Client: updated,
	})
	defer again.Close(websocket.StatusNormalClosure, "")
	stored, _ = LoadConfig(agent.StateDir)
	if stored.BrowserIdentities[0].Client.Browser != "Safari" || stored.BrowserIdentities[0].Client.Mode != "pwa" {
		t.Fatalf("reconnect client = %#v", stored.BrowserIdentities[0].Client)
	}
	list := agent.listPermissions()
	grants, _ := list["grants"].([]map[string]any)
	if len(grants) != 1 {
		t.Fatalf("list grants = %#v", list["grants"])
	}
	recorded, _ := grants[0]["client"].(BrowserClient)
	if recorded.Label != "Safari on macOS · installed app" {
		t.Fatalf("listed client = %#v", grants[0]["client"])
	}
}

func TestAuthorityCanUpdateAndRevokeGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent, _, server, authority := newAuthorityAgent(t, ctx)
	browser := newTestBrowser(t)
	declared := []string{"fs.home", "fs.readText", "fs.execFile"}
	permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "owner/tool", "capabilities": []string{"fs.home"}})
	app, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/tool", Declared: declared, Browser: browser, ReturnPending: true})
	if hello["type"] != "hello" || len(anyStrings(hello["capabilities"])) != 1 {
		t.Fatalf("hello = %#v", hello)
	}
	grant := map[string]any{"keyId": browser.keyID, "origin": "http://localhost:5179", "storeId": "owner/tool"}
	updated := permissionsRPC(t, ctx, authority, "update", "update", map[string]any{"keyId": grant["keyId"], "origin": grant["origin"], "storeId": grant["storeId"], "capabilities": []string{"fs.home", "fs.readText", "fs.execFile", "secrets.manage"}})
	if got := anyStrings(updated["grant"].(map[string]any)["capabilities"]); len(got) != 3 || stringSliceHas(got, "secrets.manage") {
		t.Fatalf("update clamped grant = %#v", got)
	}
	// The live socket is closed so the app reconnects and learns its new set.
	if _, _, err := app.Read(ctx); websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Fatalf("socket after update = %v, want 1012", err)
	}
	app.Close(websocket.StatusNormalClosure, "")
	again, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/tool", Declared: declared, Browser: browser, ReturnPending: true})
	if hello["type"] != "hello" || len(anyStrings(hello["capabilities"])) != 3 {
		t.Fatalf("hello after update = %#v", hello)
	}
	sendRequest(t, ctx, again, map[string]any{"type": "exec", "id": "exec", "file": "/bin/echo", "args": []string{"granted"}})
	for {
		reply := receive(t, ctx, again)
		if reply["type"] == "exec-error" {
			t.Fatalf("exec with granted fs.execFile = %#v", reply)
		}
		if reply["type"] == "exec-exit" {
			break
		}
	}
	revoked := permissionsRPC(t, ctx, authority, "revoke", "revoke", grant)
	if revoked["ok"] != true {
		t.Fatalf("revoke = %#v", revoked)
	}
	if _, _, err := again.Read(ctx); websocket.CloseStatus(err) != closePairingDeclined {
		t.Fatalf("socket after revoke = %v, want 4403", err)
	}
	again.Close(websocket.StatusNormalClosure, "")
	stored, _ := LoadConfig(agent.StateDir)
	for _, identity := range stored.BrowserIdentities {
		if identity.StoreID == "owner/tool" {
			t.Fatalf("revoked grant persisted: %#v", identity)
		}
	}
	third, pending := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/tool", Declared: declared, Browser: browser, ReturnPending: true})
	defer third.Close(websocket.StatusNormalClosure, "")
	if pending["type"] != "dynapp-direct-local-pending" {
		t.Fatalf("reconnect after revoke = %#v", pending)
	}
	if _, err := agent.revokeGrant(map[string]any{"keyId": "missing", "origin": "http://localhost:5179"}); err == nil {
		t.Fatal("revoking an unknown grant must fail")
	}
}

func TestAuthoritySubscriberReceivesChangeEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, server, authority := newAuthorityAgent(t, ctx)
	if result := permissionsRPC(t, ctx, authority, "sub", "subscribe"); result["subscribed"] != true {
		t.Fatalf("subscribe = %#v", result)
	}
	permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "test/app", "capabilities": []string{}})
	app, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", ReturnPending: true})
	defer app.Close(websocket.StatusNormalClosure, "")
	event := receive(t, ctx, authority)
	if event["type"] != "rpc-event" || event["service"] != "permissions" || event["event"] != "changed" {
		t.Fatalf("event after pre-approval = %#v", event)
	}
	if hello["type"] != "hello" || len(anyStrings(hello["capabilities"])) != 0 {
		t.Fatalf("hello with an empty grant = %#v", hello)
	}
}

func TestPermissionsServiceRequiresPermissionsManage(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// An ordinary app never receives permissions.manage, even when it declares
	// it and the decision grants the whole declared set.
	connection, hello := dialTestAgent(t, ctx, server, testDialOptions{Declared: []string{"fs.home", "permissions.manage"}})
	defer connection.Close(websocket.StatusNormalClosure, "")
	if capabilities := anyStrings(hello["capabilities"]); !reflect.DeepEqual(capabilities, []string{"fs.home"}) {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "rpc", "id": "list", "service": "permissions", "method": "list"})
	if reply := receive(t, ctx, connection); reply["type"] != "rpc-error" || reply["error"] != "Permission permissions.manage is not granted for this app. Review it in Dyner → Permissions." {
		t.Fatalf("permissions.list from an app = %#v", reply)
	}
	if _, err := (&Server{}).handlePermissionsRPC(nil, message{Service: "permissions", Method: "list", auth: &socketAuthentication{capabilities: []string{"fs.home"}}}); err == nil {
		t.Fatal("direct handler call without permissions.manage must fail")
	}
}

func TestDangerousPermissionWithoutGrantReturnsReviewHint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent, _, server, authority := newAuthorityAgent(t, ctx)
	permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "owner/runner", "capabilities": []string{"fs.home"}})
	app, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/runner", Declared: []string{"fs.execFile", "fs.exec", "fs.home", "net.tcp.connect"}, ReturnPending: true})
	defer app.Close(websocket.StatusNormalClosure, "")
	if capabilities := anyStrings(hello["capabilities"]); len(capabilities) != 1 || capabilities[0] != "fs.home" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	sendRequest(t, ctx, app, map[string]any{"type": "exec", "id": "exec", "file": "/bin/echo"})
	if reply := receive(t, ctx, app); reply["type"] != "exec-error" || reply["error"] != "Permission fs.execFile is not granted for this app. Review it in Dyner → Permissions." {
		t.Fatalf("exec without grant = %#v", reply)
	}
	sendRequest(t, ctx, app, map[string]any{"type": "exec", "id": "shell", "command": "echo hi"})
	if reply := receive(t, ctx, app); reply["type"] != "exec-error" || reply["error"] != "Permission fs.exec is not granted for this app. Review it in Dyner → Permissions." {
		t.Fatalf("shell exec without grant = %#v", reply)
	}
	sendRequest(t, ctx, app, map[string]any{"type": "net", "id": "tcp", "protocol": "tcp", "action": "open", "target": map[string]any{"host": "127.0.0.1", "port": 9}})
	if reply := receive(t, ctx, app); reply["error"] != "Permission net.tcp.connect is not granted for this app. Review it in Dyner → Permissions." {
		t.Fatalf("tcp without grant = %#v", reply)
	}
	sendRequest(t, ctx, app, map[string]any{"type": "rpc", "id": "secrets", "service": "secrets", "method": "list"})
	if reply := receive(t, ctx, app); reply["type"] != "rpc-error" || reply["error"] != "Permission secrets.manage is not granted for this app. Review it in Dyner → Permissions." {
		t.Fatalf("secrets without grant = %#v", reply)
	}
	stored, _ := LoadConfig(agent.StateDir)
	for _, identity := range stored.BrowserIdentities {
		if identity.StoreID == "owner/runner" && (stringSliceHas(identity.Capabilities, "fs.execFile") || len(identity.Declared) != 4) {
			t.Fatalf("stored grant = %#v", identity)
		}
	}
}

func TestCatalogPreApprovalSkipsThePendingReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agent, _, server, authority := newAuthorityAgent(t, ctx)
	declared := []string{"fs.home", "fs.readText", "fs.exec", "net.protocol.https"}
	// Undeclared and dangerous-but-included permissions: clamped to declared,
	// fs.exec granted only because the catalog ticked it, permissions.manage
	// never.
	result := permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{"storeId": "owner/notes", "capabilities": []string{"fs.home", "fs.exec", "secrets.manage", "permissions.manage"}, "expiresInMs": 5_000_000})
	if result["ok"] != true || result["storeId"] != "owner/notes" || len(anyStrings(result["capabilities"])) != 3 || stringSliceHas(anyStrings(result["capabilities"]), "permissions.manage") {
		t.Fatalf("preapprove = %#v", result)
	}
	expiresAt, _ := result["expiresAt"].(float64)
	if remaining := time.UnixMilli(int64(expiresAt)).Sub(time.Now()); remaining > time.Hour || remaining < 59*time.Minute {
		t.Fatalf("expiresInMs was not capped at one hour: %v", remaining)
	}
	list := permissionsRPC(t, ctx, authority, "list", "list")
	preapproved, _ := list["preapproved"].([]any)
	if len(preapproved) != 1 || preapproved[0].(map[string]any)["storeId"] != "owner/notes" {
		t.Fatalf("list.preapproved = %#v", list["preapproved"])
	}
	// A newer call replaces the older one.
	permissionsRPC(t, ctx, authority, "pre2", "preapprove", map[string]any{"storeId": "owner/notes", "capabilities": []string{"fs.home", "fs.exec", "secrets.manage"}})
	browser := newTestBrowser(t)
	app, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/notes", Declared: declared, Browser: browser, ReturnPending: true})
	defer app.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || !reflect.DeepEqual(anyStrings(hello["capabilities"]), []string{"fs.exec", "fs.home"}) {
		t.Fatalf("hello with pre-approval = %#v", hello)
	}
	list = permissionsRPC(t, ctx, authority, "list2", "list")
	if len(list["preapproved"].([]any)) != 0 || len(list["pending"].([]any)) != 0 || len(list["grants"].([]any)) != 2 {
		t.Fatalf("list after consumption = %#v", list)
	}
	stored, _ := LoadConfig(agent.StateDir)
	var identity *BrowserIdentity
	for index := range stored.BrowserIdentities {
		if stored.BrowserIdentities[index].StoreID == "owner/notes" {
			identity = &stored.BrowserIdentities[index]
		}
	}
	if identity == nil || identity.ApprovedAt == 0 || !reflect.DeepEqual(identity.Capabilities, []string{"fs.exec", "fs.home"}) || len(identity.Declared) != 4 {
		t.Fatalf("persisted identity = %#v", stored.BrowserIdentities)
	}
	// Without a dangerous permission ticked it is not granted.
	permissionsRPC(t, ctx, authority, "pre3", "preapprove", map[string]any{"storeId": "owner/other", "capabilities": []string{"fs.home", "fs.readText"}})
	other, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/other", Declared: declared, ReturnPending: true})
	defer other.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || !reflect.DeepEqual(anyStrings(hello["capabilities"]), []string{"fs.home", "fs.readText"}) {
		t.Fatalf("hello without dangerous grant = %#v", hello)
	}
	// An expired pre-approval falls back to the Dyner-owned pending flow.
	state := agent.permissions()
	state.mu.Lock()
	state.preapproved["owner/stale"] = preApproval{StoreID: "owner/stale", Capabilities: []string{"fs.home"}, ExpiresAt: time.Now().Add(-time.Second)}
	state.mu.Unlock()
	stale, pending := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179", StoreID: "owner/stale", Declared: declared, ReturnPending: true})
	defer stale.Close(websocket.StatusNormalClosure, "")
	if pending["type"] != "dynapp-direct-local-pending" {
		t.Fatalf("expired pre-approval = %#v", pending)
	}
	if _, err := agent.preapprovePermissions(map[string]any{"storeId": "bad id", "capabilities": []any{}}); err == nil {
		t.Fatal("invalid store id must be rejected")
	}
	if _, err := agent.preapprovePermissions(map[string]any{"storeId": "owner/x"}); err == nil {
		t.Fatal("missing capabilities must be rejected")
	}
}

func TestRelayAndLANGrantsIntersectIdentityWithManifest(t *testing.T) {
	identities := []BrowserIdentity{
		{KeyID: "authority", Origin: "https://dynapp.io", Authority: true, Capabilities: []string{"permissions.manage", "apps.manage"}},
		{KeyID: "browser-a", Origin: "https://dynapp.io", Capabilities: []string{"fs.remote", "fs.exec", "net.protocol.https"}, Connect: map[string][]string{"https": {"https://api.example/"}}},
		{KeyID: "browser-b", Origin: "https://dynapp.io", Capabilities: []string{"fs.remote", "clipboard.write"}},
	}
	resolve := func(storeID string) (resolvedApp, bool) {
		if storeID == "owner/notes" {
			return resolvedApp{StoreID: storeID, Name: "Notes", Declared: []string{"fs.remote", "fs.exec", "clipboard.write"}}, true
		}
		return resolvedApp{}, false
	}
	// Named identity and resolvable manifest: identity ∩ manifest.
	auth := relayGrantFor(identities, message{KeyID: "browser-a", App: &directLocalApp{StoreID: "owner/notes"}}, resolve)
	if !reflect.DeepEqual(auth.capabilities, []string{"fs.exec", "fs.remote"}) || auth.storeID != "owner/notes" || auth.appName != "Notes" || auth.keyID != "browser-a" {
		t.Fatalf("relay grant = %#v", auth)
	}
	// Unresolvable manifest: identity capabilities alone, identity connect list.
	auth = relayGrantFor(identities, message{KeyID: "browser-a", App: &directLocalApp{StoreID: "owner/unknown"}}, resolve)
	if !reflect.DeepEqual(auth.capabilities, []string{"fs.exec", "fs.remote", "net.protocol.https"}) || len(auth.connect["https"]) != 1 {
		t.Fatalf("relay grant without manifest = %#v", auth)
	}
	// No identity named: the common subset of every synced identity, never the
	// authority's set and never the environment ceiling.
	auth = relayGrantFor(identities, message{}, resolve)
	if !reflect.DeepEqual(auth.capabilities, []string{"fs.remote"}) || auth.keyID != "" || auth.connect != nil {
		t.Fatalf("relay grant without identity = %#v", auth)
	}
	if auth = relayGrantFor(nil, message{}, resolve); len(auth.capabilities) != 0 {
		t.Fatalf("relay grant without identities = %#v", auth)
	}
	// LAN sockets go through authForIdentity: a Dyner-synced identity (no
	// store id) is intersected with the resolved manifest.
	synced := BrowserIdentity{KeyID: "browser-b", Origin: "https://dynapp.io", Capabilities: []string{"fs.remote", "clipboard.write", "fs.exec"}}
	app := resolvedApp{StoreID: "owner/notes", Name: "Notes", Declared: []string{"fs.remote", "fs.exec"}}
	lan := authForIdentity(Config{EnvironmentID: "env_1"}, synced, &app)
	if !reflect.DeepEqual(lan.capabilities, []string{"fs.exec", "fs.remote"}) || lan.storeID != "owner/notes" {
		t.Fatalf("LAN grant = %#v", lan)
	}
	if alone := authForIdentity(Config{}, synced, nil); !reflect.DeepEqual(alone.capabilities, []string{"clipboard.write", "fs.exec", "fs.remote"}) {
		t.Fatalf("LAN grant without manifest = %#v", alone)
	}
}

func TestAuthorityOriginsAndReviewURLConfig(t *testing.T) {
	config := Config{DynerBaseURL: "https://dynapp.io", AuthorityOrigins: []string{"http://localhost:3000"}}
	if !config.isAuthorityOrigin("https://dynapp.io") || !config.isAuthorityOrigin("https://amitbet-dyner.dynapp.io") || !config.isAuthorityOrigin("http://localhost:3000") || config.isAuthorityOrigin("https://amitbet-notes.dynapp.io") || config.isAuthorityOrigin("https://evil.example") {
		t.Fatal("authority origin classification is wrong")
	}
	if got := config.catalogHostedOrigin(); got != "https://amitbet-dyner.dynapp.io" {
		t.Fatalf("catalog hosted origin = %q", got)
	}
	if got := config.reviewURL("req_1"); got != "https://dynapp.io/app/amit-bet/dyner#/permissions?request=req_1" {
		t.Fatalf("reviewUrl = %q", got)
	}
	config.CatalogAppID = "someone/catalog"
	if got := config.reviewURL("req_2"); got != "https://dynapp.io/app/someone/catalog#/permissions?request=req_2" {
		t.Fatalf("configured catalog reviewUrl = %q", got)
	}
	if classifyOrigin(Config{DynerBaseURL: "https://dynapp.io", AuthorityOrigins: []string{"https://dev.dyner.example"}}, nil, "https://dev.dyner.example") != originHosted {
		t.Fatal("configured authority origins must pass the transport allowlist")
	}
	if got := suggestedPermissions([]string{"fs.exec", "fs.home", "secrets.manage", "agent.session", "apps.publish", "net.udp.bind", "clipboard.write"}); !reflect.DeepEqual(got, []string{"clipboard.write", "fs.exec", "fs.home", "net.udp.bind"}) {
		t.Fatalf("suggested = %#v", got)
	}
}

func TestHostedCatalogOriginIsRecordedAsAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dyner := permissionTestDyner(t)
	defer dyner.Close()
	agent := &Server{StateDir: t.TempDir(), Config: Config{DynerBaseURL: dyner.URL, AppDomain: "dynapp.io"}, DynerHTTPClient: dyner.Client()}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()

	authority, hello := dialTestAgent(t, ctx, server, testDialOptions{
		Origin:        "https://amitbet-dyner.dynapp.io",
		StoreID:       "amit-bet/dyner",
		Declared:      []string{"apps.manage"},
		ReturnPending: true,
	})
	defer authority.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || !stringSliceHas(anyStrings(hello["capabilities"]), permissionsManage) {
		t.Fatalf("hosted catalog authority handshake = %#v", hello)
	}
	result := permissionsRPC(t, ctx, authority, "pre", "preapprove", map[string]any{
		"storeId": "amit-bet/notes", "capabilities": []string{"fs.readText"},
	})
	if result["ok"] != true {
		t.Fatalf("hosted catalog preapproval = %#v", result)
	}
	app, appHello := dialTestAgent(t, ctx, server, testDialOptions{
		Origin:        "https://amitbet-notes.dynapp.io",
		StoreID:       "amit-bet/notes",
		ReturnPending: true,
	})
	defer app.Close(websocket.StatusNormalClosure, "")
	if appHello["type"] != "hello" || !reflect.DeepEqual(anyStrings(appHello["capabilities"]), []string{"fs.readText"}) {
		t.Fatalf("hosted catalog app handoff = %#v", appHello)
	}
	stored, err := LoadConfig(agent.StateDir)
	var hostedAuthority *BrowserIdentity
	for index := range stored.BrowserIdentities {
		if stored.BrowserIdentities[index].Origin == "https://amitbet-dyner.dynapp.io" {
			hostedAuthority = &stored.BrowserIdentities[index]
		}
	}
	if err != nil || hostedAuthority == nil || !hostedAuthority.Authority {
		t.Fatalf("hosted catalog authority identity = %#v, err = %v", stored.BrowserIdentities, err)
	}
}

func TestVersionOneInitCannotCreateAnIdentity(t *testing.T) {
	server := httptest.NewServer((&Server{StateDir: t.TempDir(), autoApprovePairings: true}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	browser := newTestBrowser(t)
	headers := http.Header{"Origin": []string{"http://localhost:5179"}}
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+RemotePath, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{"type": "dynapp-direct-local-init", "version": 1, "publicKeyJwk": browser.key})
	challenge := receive(t, ctx, connection)
	nonce, _ := challenge["nonce"].(string)
	sendRequest(t, ctx, connection, map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": browser.keyID, "origin": "http://localhost:5179", "signature": browser.sign(t, nonce, "http://localhost:5179")})
	_, _, err = connection.Read(ctx)
	if websocket.CloseStatus(err) != closeIdentityRejected {
		t.Fatalf("v1 unknown identity close = %v, want 4401", err)
	}
}

func TestFrameAppIDMustMatchPairedApp(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server, testDialOptions{StoreID: "owner/notes"})
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{"type": "rpc", "id": "mismatch", "service": "sessions", "method": "list", "appId": "owner/other"})
	if reply := receive(t, ctx, connection); reply["type"] != "rpc-error" || !strings.Contains(reply["error"].(string), "does not match") {
		t.Fatalf("mismatched appId reply = %#v", reply)
	}
	for _, appID := range []string{"owner/notes", "notes", ""} {
		frame := map[string]any{"type": "rpc", "id": "ok-" + appID, "service": "sessions", "method": "list"}
		if appID != "" {
			frame["appId"] = appID
		}
		sendRequest(t, ctx, connection, frame)
		if reply := receive(t, ctx, connection); reply["type"] != "rpc-result" {
			t.Fatalf("appId %q reply = %#v", appID, reply)
		}
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "exec", "id": "exec-mismatch", "file": "/bin/echo", "appId": "owner/other"})
	if reply := receive(t, ctx, connection); reply["type"] != "exec-error" {
		t.Fatalf("mismatched exec reply = %#v", reply)
	}
}

func TestSessionsAreKeyedByTheBoundApp(t *testing.T) {
	agent := newTestServer(t)
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server, testDialOptions{StoreID: "owner/notes"})
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{"type": "rpc", "id": "upsert", "service": "sessions", "method": "upsert", "appId": "notes", "args": []any{map[string]any{"sessionId": "s1", "value": map[string]any{"a": 1}}}})
	if reply := receive(t, ctx, connection); reply["type"] != "rpc-result" {
		t.Fatalf("upsert = %#v", reply)
	}
	bound, err := agent.handleSessionsRPC(message{AppID: "owner/notes", Method: "list"})
	if err != nil || len(bound.([]sessionRecord)) != 1 {
		t.Fatalf("sessions under bound id = %#v, %v", bound, err)
	}
	slug, err := agent.handleSessionsRPC(message{AppID: "notes", Method: "list"})
	if err != nil || len(slug.([]sessionRecord)) != 0 {
		t.Fatalf("sessions under frame slug = %#v, %v", slug, err)
	}
}

func TestFSRemoteNoLongerGrantsExecFileOverTheWire(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, hello := dialTestAgent(t, ctx, server, testDialOptions{Declared: []string{"fs.remote"}})
	defer connection.Close(websocket.StatusNormalClosure, "")
	if capabilities, _ := hello["capabilities"].([]any); len(capabilities) != 1 || capabilities[0] != "fs.remote" {
		t.Fatalf("capabilities = %#v", hello["capabilities"])
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "exec", "id": "exec", "file": "/bin/echo", "args": []string{"nope"}})
	if reply := receive(t, ctx, connection); reply["type"] != "exec-error" {
		t.Fatalf("exec with fs.remote = %#v", reply)
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "fs", "id": "open", "method": "openPath", "args": []any{"/"}})
	if reply := receive(t, ctx, connection); reply["type"] != "fs-error" {
		t.Fatalf("openPath with fs.remote = %#v", reply)
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "fs", "id": "home", "method": "home", "args": []any{}})
	if reply := receive(t, ctx, connection); reply["type"] != "fs-result" {
		t.Fatalf("home with fs.remote = %#v", reply)
	}
}

func TestHostedOriginDerivesCapabilitiesFromDynerManifest(t *testing.T) {
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("pairing lookup must not carry credentials: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/apps/amit-bet/notes" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"app":           map[string]any{"id": "amit-bet/notes", "name": "Notes"},
			"hostedOrigins": []string{"https://amitbet-notes.dynapp.io"},
			"latestRevision": map[string]any{
				"id": "rev_1",
				"manifest": map[string]any{
					"name": "Notes",
					"backendPermissions": []any{
						"fs.readText",
						map[string]any{"permission": "fs.exec", "request": "on-demand"},
						map[string]any{"permission": "net.protocol.https", "connect": []any{"https://api.example/v1/"}},
					},
				},
			},
		})
	}))
	defer dyner.Close()
	agent := &Server{StateDir: t.TempDir(), autoApprovePairings: true, Config: Config{DynerBaseURL: dyner.URL, AppDomain: "dynapp.io"}, DynerHTTPClient: dyner.Client()}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The init's declaredPermissions are ignored for hosted origins.
	connection, hello := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Declared: []string{"fs.remote", "secrets.manage"}})
	defer connection.Close(websocket.StatusNormalClosure, "")
	capabilities, _ := hello["capabilities"].([]any)
	if len(capabilities) != 3 || capabilities[0] != "fs.exec" || capabilities[1] != "fs.readText" || capabilities[2] != "net.protocol.https" {
		t.Fatalf("hosted capabilities = %#v", capabilities)
	}
	stored, _ := LoadConfig(agent.StateDir)
	if len(stored.BrowserIdentities) != 1 || len(stored.BrowserIdentities[0].Connect["https"]) != 1 || stored.BrowserIdentities[0].Connect["https"][0] != "https://api.example/v1/" {
		t.Fatalf("stored connect allowlist = %#v", stored.BrowserIdentities)
	}
	// A subdomain that Dyner does not list as a hosted origin is rejected.
	other, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+RemotePath, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://amitbet-other.dynapp.io"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(websocket.StatusNormalClosure, "")
	browser := newTestBrowser(t)
	sendRequest(t, ctx, other, map[string]any{"type": "dynapp-direct-local-init", "version": 2, "publicKeyJwk": browser.key, "app": map[string]any{"storeId": "amit-bet/notes"}})
	challenge := receive(t, ctx, other)
	nonce, _ := challenge["nonce"].(string)
	sendRequest(t, ctx, other, map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": browser.keyID, "origin": "https://amitbet-other.dynapp.io", "signature": browser.sign(t, nonce, "https://amitbet-other.dynapp.io")})
	if _, _, err := other.Read(ctx); websocket.CloseStatus(err) != closeIdentityRejected {
		t.Fatalf("mismatched hosted origin close = %v", err)
	}
}

func TestLegacyHostedOriginFallbackWithoutHostedOrigins(t *testing.T) {
	config := Config{DynerBaseURL: "https://dynapp.io"}
	if !legacyHostedOriginMatches(config, "https://amitbet-notes.dynapp.io", "amit-bet/notes") {
		t.Fatal("owner-without-hyphens subdomain should match")
	}
	if !legacyHostedOriginMatches(config, "https://dynapp.io", "amit-bet/dyner") {
		t.Fatal("the Dyner base origin (catalog) should match")
	}
	if legacyHostedOriginMatches(config, "https://amit-bet-notes.dynapp.io", "amit-bet/notes") || legacyHostedOriginMatches(config, "https://amitbet-other.dynapp.io", "amit-bet/notes") {
		t.Fatal("other subdomains must not match")
	}
}

func TestHTTPConnectAllowlistComesFromIdentityAndBlocksPrivateTargets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer upstream.Close()
	server := &Server{}
	identity := &socketAuthentication{keyID: "k", connect: map[string][]string{"http": {"http://example.test/"}}, capabilities: []string{"net.protocol.http"}}
	// The request's own connect list is ignored for paired sockets.
	if _, err := server.handleHTTPRPC(context.Background(), message{Method: "request", auth: identity, Args: []any{map[string]any{"url": upstream.URL, "connect": []any{upstream.URL + "/"}}}}); err == nil {
		t.Fatal("request-supplied connect list was honored for a paired socket")
	}
	// Loopback targets are refused unless the identity allowlist names the host.
	named := &socketAuthentication{keyID: "k", connect: map[string][]string{"http": {upstream.URL + "/"}}, capabilities: []string{"net.protocol.http"}}
	result, err := server.handleHTTPRPC(context.Background(), message{Method: "request", auth: named, Args: []any{map[string]any{"url": upstream.URL}}})
	if err != nil || result.(map[string]any)["text"] != "ok" {
		t.Fatalf("named loopback host = %#v, %v", result, err)
	}
	for _, target := range []string{"http://127.0.0.1:9/", "http://localhost:9/", "http://10.0.0.8/", "http://169.254.169.254/latest", "http://192.168.1.1/"} {
		if err := assertHTTPTargetAllowed(mustURL(t, target), []string{"http://127.0.0.1:9/", "http://localhost:9/", "http://10.0.0.8/", "http://169.254.169.254/", "http://192.168.1.1/"}); err != nil {
			t.Fatalf("exact host in allowlist should be permitted: %s %v", target, err)
		}
		if err := assertHTTPTargetAllowed(mustURL(t, target), []string{"http://public.example/"}); err == nil {
			t.Fatalf("private target %s allowed without being named", target)
		}
	}
}

func TestSafeHTTPTransportRechecksResolvedAddresses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	address := listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocked := safeHTTPTransport([]string{"http://public.example/"})
	if _, err := blocked.DialContext(ctx, "tcp", address); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("dialer allowed a loopback address that was not named: %v", err)
	}
	allowed := safeHTTPTransport([]string{"http://" + address + "/"})
	connection, err := allowed.DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("named loopback host was blocked: %v", err)
	}
	_ = connection.Close()
}

func TestNetworkFileCopiesRequireLocalFilesystemCapabilities(t *testing.T) {
	server := &Server{}
	auth := &socketAuthentication{capabilities: []string{"net.protocol.sftp"}}
	if _, err := server.handleNetworkFileRPC(context.Background(), nil, message{Service: "sftp", Method: "copyToLocal", auth: auth, Args: []any{map[string]any{"host": "files.example"}, "/remote", "/tmp/local"}}); err == nil || !strings.Contains(err.Error(), "fs.writeBase64") {
		t.Fatalf("copyToLocal without fs.writeBase64 = %v", err)
	}
	if _, err := server.handleNetworkFileRPC(context.Background(), nil, message{Service: "sftp", Method: "copyFromLocal", auth: auth, Args: []any{map[string]any{"host": "files.example"}, "/tmp/local", "/remote"}}); err == nil || !strings.Contains(err.Error(), "fs.readBase64") {
		t.Fatalf("copyFromLocal without fs.readBase64 = %v", err)
	}
	if _, err := server.handleNetworkFileRPC(context.Background(), nil, message{Service: "sftp", Method: "list", auth: auth, Args: []any{map[string]any{"host": "files.example", "identityFile": "/home/me/.ssh/id"}, "/"}}); err == nil || !strings.Contains(err.Error(), "fs.readText") {
		t.Fatalf("identityFile without fs.readText = %v", err)
	}
	if _, err := server.handleHTTPRPC(context.Background(), message{Method: "download", auth: &socketAuthentication{capabilities: []string{"net.protocol.https"}}, Args: []any{map[string]any{"url": "https://example.test/file", "connect": []any{"https://example.test/"}}}}); err == nil || !strings.Contains(err.Error(), "fs.writeBase64") {
		t.Fatalf("http.download without fs.writeBase64 = %v", err)
	}
	search := &socketAuthentication{capabilities: []string{"search.files"}}
	if _, err := (&Server{StateDir: t.TempDir()}).handleFileSearchRPC(context.Background(), message{Method: "open", auth: search, Args: []any{"/tmp"}}); err == nil || !strings.Contains(err.Error(), "fs.openPath") {
		t.Fatalf("fileSearch.open without fs.openPath = %v", err)
	}
	if _, err := (&Server{StateDir: t.TempDir()}).handleFileSearchRPC(context.Background(), message{Method: "rebuild", auth: search, Args: []any{map[string]any{}}}); err == nil || !strings.Contains(err.Error(), "fs.list") {
		t.Fatalf("fileSearch.rebuild without fs.list = %v", err)
	}
}

func TestSettingsAPIEnforcesHostOriginAndCSRF(t *testing.T) {
	agent := &Server{StateDir: t.TempDir()}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	post := func(path, body string, mutate func(*http.Request)) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(request)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response
	}
	if response := rawGet(t, server.URL+"/api/settings/status", func(r *http.Request) { r.Host = "agent.evil.example" }); response.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound Host status = %d", response.StatusCode)
	}
	if response := rawGet(t, server.URL+"/api/settings/status", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin status = %d", response.StatusCode)
	}
	if response := rawGet(t, server.URL+"/api/settings/status", func(r *http.Request) { r.Header.Set("Origin", server.URL) }); response.StatusCode != http.StatusOK {
		t.Fatalf("own Origin status = %d", response.StatusCode)
	}
	if response := rawGet(t, server.URL+"/api/settings/pairings", nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed pairings route status = %d, want 404", response.StatusCode)
	}
	// Browser callers must send JSON and the CSRF token from the settings page.
	if response := post("/api/settings/remote-environments/tailscale", `{}`, func(r *http.Request) { r.Header.Set("Origin", server.URL) }); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("missing JSON content type status = %d", response.StatusCode)
	}
	if response := post("/api/settings/remote-environments/tailscale", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", server.URL)
		r.Header.Set("Content-Type", "application/json")
	}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", response.StatusCode)
	}
	indexResponse, err := http.Get(server.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	var page strings.Builder
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := indexResponse.Body.Read(buffer)
		page.Write(buffer[:n])
		if readErr != nil {
			break
		}
	}
	_ = indexResponse.Body.Close()
	match := regexp.MustCompile(`name="dynapp-settings-csrf" content="([^"]+)"`).FindStringSubmatch(page.String())
	if len(match) != 2 || match[1] == settingsCSRFPlaceholder {
		t.Fatalf("settings page did not embed a CSRF token: %v", match)
	}
	if response := post("/api/settings/remote-environments/tailscale", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", server.URL)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(settingsCSRFHeader, match[1])
	}); response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("valid CSRF status = %d, want 501 from the guarded handler", response.StatusCode)
	}
	if response := post("/api/settings/remote-environments/tailscale", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(settingsCSRFHeader, match[1])
	}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin with CSRF status = %d", response.StatusCode)
	}
	// Non-browser callers (no Origin) are accepted with a JSON body.
	if response := post("/api/settings/remote-environments/tailscale", `{}`, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }); response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("launcher-style request status = %d", response.StatusCode)
	}
	if response := post("/external-open", `{"appId":"viewer","paths":[]}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("external-open without JSON status = %d", response.StatusCode)
	}
	if response := post("/external-open", `{"appId":"viewer","paths":[]}`, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://evil.example")
	}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("external-open from a foreign origin status = %d", response.StatusCode)
	}
}

func TestPermissionCatalogMirrorsSharedCatalog(t *testing.T) {
	if permissionGroupID("fs.exec") != "execute" || permissionGroupID("dialog.pickPath") != "filesystem" || permissionGroupID("net.tcp.connect") != "network" || permissionGroupID("apps.install") != "store" || permissionGroupID("clipboard.write") != "device" {
		t.Fatal("group mapping drifted from shared/permissions/catalog.js")
	}
	if permissionDanger("fs.exec") != "high" || permissionDanger("net.tcp.listen") != "high" || permissionDanger("fs.writeText") != "high" || permissionDanger("fs.readText") != "medium" || permissionDanger("notification.show") != "low" {
		t.Fatal("danger tiers drifted from shared/permissions/catalog.js")
	}
	groups := groupPermissions([]string{"fs.readText", "fs.writeText", "clipboard.write", "fs.exec"})
	if len(groups) != 3 || groups[0].ID != "filesystem" || groups[0].Danger != "high" || groups[1].ID != "execute" || groups[2].ID != "device" {
		t.Fatalf("groups = %#v", groups)
	}
	if permissionTitle("fs.execFile") != "Fs Exec File" {
		t.Fatalf("title = %q", permissionTitle("fs.execFile"))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
