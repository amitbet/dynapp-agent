package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

const nativeNotesOrigin = "https://amitbet-notes.dynapp.io"

// nativeTestAgent is an agent with one installed native app whose Dyner
// lookups go to the shared permission test server.
func nativeTestAgent(t *testing.T, app NativeApp) (*Server, string) {
	t.Helper()
	dyner := permissionTestDyner(t)
	t.Cleanup(dyner.Close)
	previous := verifyNativePeer
	verifyNativePeer = func(net.Conn, NativeApp) error { return nil }
	t.Cleanup(func() { verifyNativePeer = previous })
	agent := &Server{
		StateDir:        t.TempDir(),
		Config:          Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: dyner.URL, AppDomain: "dynapp.io", NativeApps: NativeAppList{app}},
		DynerHTTPClient: dyner.Client(),
	}
	return agent, dyner.URL
}

func notesNativeApp() NativeApp {
	return NativeApp{StoreID: "amit-bet/notes", Name: "Notes", Origin: nativeNotesOrigin, URL: nativeNotesOrigin + "/", Path: "/Applications/Notes.app"}
}

// dialNative connects a host-side frame socket to the agent over a pipe.
func dialNative(t *testing.T, agent *Server, storeID, purpose string) *nativeFrameSocket {
	t.Helper()
	hostSide, agentSide := net.Pipe()
	go agent.serveNativeConnection(agentSide)
	host := newNativeFrameSocket(hostSide)
	t.Cleanup(func() { _ = hostSide.Close() })
	writeNative(t, host, map[string]any{"type": "native-host-hello", "version": 1, "storeId": storeID, "purpose": purpose})
	return host
}

func writeNative(t *testing.T, socket *nativeFrameSocket, value any) {
	t.Helper()
	if err := socket.Write(context.Background(), websocket.MessageText, mustJSON(value)); err != nil {
		t.Fatalf("write native frame: %v", err)
	}
}

func readNative(t *testing.T, socket *nativeFrameSocket) map[string]any {
	t.Helper()
	_ = socket.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data, err := socket.Read(context.Background())
	if err != nil {
		t.Fatalf("read native frame: %v", err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("unexpected binary native frame")
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("native frame is not JSON: %v", err)
	}
	return frame
}

func groupPermissionsOf(frame map[string]any) []string {
	var permissions []string
	groups, _ := frame["groups"].([]any)
	for _, group := range groups {
		object, _ := group.(map[string]any)
		permissions = append(permissions, anyStrings(object["permissions"])...)
	}
	return normalizePermissionSet(permissions)
}

func testBrowserIdentity(t *testing.T, origin string) BrowserIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	coordinate := func(value []byte) string {
		padded := make([]byte, 32)
		copy(padded[32-len(value):], value)
		return base64.RawURLEncoding.EncodeToString(padded)
	}
	jwk := BrowserJWK{KTY: "EC", CRV: "P-256", X: coordinate(key.X.Bytes()), Y: coordinate(key.Y.Bytes())}
	return BrowserIdentity{KeyID: BrowserKeyID(jwk), PublicKeyJWK: jwk, Origin: origin, Capabilities: []string{"fs.readText"}}
}

func TestNativeFrameSocketRoundTripsTextAndBinary(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	writer, reader := newNativeFrameSocket(left), newNativeFrameSocket(right)
	go func() {
		_ = writer.Write(context.Background(), websocket.MessageText, []byte(`{"type":"ping"}`))
		_ = writer.Write(context.Background(), websocket.MessageBinary, []byte{0, 1, 2, 255})
		_ = writer.Write(context.Background(), websocket.MessageText, nil)
	}()
	kind, data, err := reader.Read(context.Background())
	if err != nil || kind != websocket.MessageText || string(data) != `{"type":"ping"}` {
		t.Fatalf("text frame = %v %q %v", kind, data, err)
	}
	kind, data, err = reader.Read(context.Background())
	if err != nil || kind != websocket.MessageBinary || string(data) != "\x00\x01\x02\xff" {
		t.Fatalf("binary frame = %v %q %v", kind, data, err)
	}
	kind, data, err = reader.Read(context.Background())
	if err != nil || kind != websocket.MessageText || len(data) != 0 {
		t.Fatalf("empty frame = %v %q %v", kind, data, err)
	}
}

func TestDamagedNativeAppEntriesDoNotBlockConfigLoad(t *testing.T) {
	dir := t.TempDir()
	identity := testBrowserIdentity(t, "https://amitbet-notes.dynapp.io")
	identityJSON, _ := json.Marshal(identity)
	raw := `{"schemaVersion":1,"browserIdentities":[` + string(identityJSON) + `],"nativeApps":[` +
		`{"storeId":"amit-bet/notes","name":"Notes","origin":"https://amitbet-notes.dynapp.io","path":"/A.app","capabilities":["fs.readText"]},` +
		`{"storeId":"not a store id","origin":"https://x.dynapp.io","path":"/B.app"},` +
		`{"storeId":"amit-bet/other","capabilities":42},` +
		`"garbage"]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig with damaged native entries: %v", err)
	}
	if len(config.BrowserIdentities) != 1 {
		t.Fatalf("browser identities were lost: %#v", config.BrowserIdentities)
	}
	if len(config.NativeApps) != 1 || config.NativeApps[0].StoreID != "amit-bet/notes" {
		t.Fatalf("native apps = %#v", config.NativeApps)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"schemaVersion":1,"nativeApps":{"not":"a list"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err != nil {
		t.Fatalf("LoadConfig with a non-list nativeApps: %v", err)
	}
}

func TestNativePairingIsAskedInTheAppWindow(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	host := dialNative(t, agent, "amit-bet/notes", "app")
	request := readNative(t, host)
	if request["type"] != "native-host-permission-request" || request["kind"] != "pairing" || request["appName"] != "Notes" || request["origin"] != nativeNotesOrigin {
		t.Fatalf("permission request = %#v", request)
	}
	if got := groupPermissionsOf(request); strings.Join(got, ",") != "fs.exec,fs.readText,net.protocol.https" {
		t.Fatalf("requested permissions = %v", got)
	}
	writeNative(t, host, map[string]any{"type": "native-host-permission-decision", "requestId": request["requestId"], "capabilities": []string{"fs.readText", "secrets.manage"}})
	ready := readNative(t, host)
	if ready["type"] != "native-host-ready" || strings.Join(anyStrings(ready["capabilities"]), ",") != "fs.readText" {
		t.Fatalf("ready = %#v", ready)
	}
	writeNative(t, host, map[string]any{"type": "hello", "protocol": ProtocolVersion})
	hello := readNative(t, host)
	if hello["type"] != "hello" || strings.Join(anyStrings(hello["capabilities"]), ",") != "fs.readText" {
		t.Fatalf("hello = %#v", hello)
	}
	app, _ := agent.nativeApp("amit-bet/notes")
	if app.ApprovedAt == 0 || strings.Join(app.Declared, ",") != "fs.exec,fs.readText,net.protocol.https" || strings.Join(app.Capabilities, ",") != "fs.readText" {
		t.Fatalf("stored grant = %#v", app)
	}
	if len(app.Connect["https"]) != 1 {
		t.Fatalf("connect list was not stored: %#v", app.Connect)
	}
}

func TestNativeDeltaAsksOnlyNewPermissionsAndRemembersADecline(t *testing.T) {
	app := notesNativeApp()
	app.ApprovedAt = time.Now().UnixMilli()
	app.Capabilities = []string{"fs.readText"}
	app.Declared = []string{"fs.readText"}
	agent, _ := nativeTestAgent(t, app)
	host := dialNative(t, agent, app.StoreID, "app")
	request := readNative(t, host)
	if request["kind"] != "delta" || strings.Join(groupPermissionsOf(request), ",") != "fs.exec,net.protocol.https" {
		t.Fatalf("delta request = %#v", request)
	}
	writeNative(t, host, map[string]any{"type": "native-host-permission-decision", "requestId": request["requestId"], "capabilities": nil})
	ready := readNative(t, host)
	if strings.Join(anyStrings(ready["capabilities"]), ",") != "fs.readText" {
		t.Fatalf("ready after decline = %#v", ready)
	}
	// The declined permissions count as reviewed, so a reconnect is not asked.
	again := dialNative(t, agent, app.StoreID, "app")
	if frame := readNative(t, again); frame["type"] != "native-host-ready" {
		t.Fatalf("reconnect after decline = %#v", frame)
	}
}

func TestNativeConnectionsShareOnePendingDecision(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	first := dialNative(t, agent, "amit-bet/notes", "app")
	firstRequest := readNative(t, first)
	second := dialNative(t, agent, "amit-bet/notes", "app")
	secondRequest := readNative(t, second)
	if firstRequest["requestId"] == "" || firstRequest["requestId"] != secondRequest["requestId"] {
		t.Fatalf("requests were not shared: %v vs %v", firstRequest["requestId"], secondRequest["requestId"])
	}
	writeNative(t, second, map[string]any{"type": "native-host-permission-decision", "requestId": secondRequest["requestId"], "capabilities": []string{"fs.exec"}})
	for _, host := range []*nativeFrameSocket{first, second} {
		ready := readNative(t, host)
		if ready["type"] != "native-host-ready" || strings.Join(anyStrings(ready["capabilities"]), ",") != "fs.exec" {
			t.Fatalf("ready = %#v", ready)
		}
	}
}

func TestDynerCanDecideANativePendingRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	agent, _, _, authority := newAuthorityAgent(t, ctx)
	previous := verifyNativePeer
	verifyNativePeer = func(net.Conn, NativeApp) error { return nil }
	t.Cleanup(func() { verifyNativePeer = previous })
	agent.upsertNativeApp(notesNativeApp())
	host := dialNative(t, agent, "amit-bet/notes", "app")
	request := readNative(t, host)
	list := permissionsRPC(t, ctx, authority, "list", "list")
	pending, _ := list["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["id"] != request["requestId"] {
		t.Fatalf("Dyner pending list = %#v", list["pending"])
	}
	permissionsRPC(t, ctx, authority, "decide", "decide", map[string]any{"id": request["requestId"], "capabilities": []string{"fs.readText"}})
	if ready := readNative(t, host); strings.Join(anyStrings(ready["capabilities"]), ",") != "fs.readText" {
		t.Fatalf("ready after Dyner decision = %#v", ready)
	}
}

func TestNativeConnectionWithoutDynerKeepsTheStoredGrant(t *testing.T) {
	app := notesNativeApp()
	app.ApprovedAt = time.Now().UnixMilli()
	app.Capabilities = []string{"fs.readText"}
	app.Declared = []string{"fs.readText"}
	agent, _ := nativeTestAgent(t, app)
	agent.Config.DynerBaseURL = "http://127.0.0.1:1"
	agent.DynerHTTPClient = nil
	host := dialNative(t, agent, app.StoreID, "app")
	if ready := readNative(t, host); ready["type"] != "native-host-ready" || strings.Join(anyStrings(ready["capabilities"]), ",") != "fs.readText" {
		t.Fatalf("offline ready = %#v", ready)
	}
}

func TestNativeHelloForAnUninstalledAppIsRejected(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	host := dialNative(t, agent, "amit-bet/other", "app")
	if frame := readNative(t, host); frame["type"] != "native-host-error" {
		t.Fatalf("frame = %#v", frame)
	}
}

func TestNativeReviewReplacesTheGrantAndClosesAppSockets(t *testing.T) {
	app := notesNativeApp()
	app.ApprovedAt = time.Now().UnixMilli()
	app.Capabilities = []string{"fs.readText"}
	app.Declared = []string{"fs.exec", "fs.readText", "net.protocol.https"}
	agent, _ := nativeTestAgent(t, app)
	page := dialNative(t, agent, app.StoreID, "app")
	if frame := readNative(t, page); frame["type"] != "native-host-ready" {
		t.Fatalf("page ready = %#v", frame)
	}
	writeNative(t, page, map[string]any{"type": "hello", "protocol": ProtocolVersion})
	readNative(t, page)

	control := dialNative(t, agent, app.StoreID, "control")
	ready := readNative(t, control)
	script, _ := ready["bootstrapScript"].(string)
	if ready["type"] != "native-host-ready" || ready["url"] != nativeNotesOrigin+"/" || !strings.Contains(script, `"storeId":"amit-bet/notes"`) || strings.Contains(script, "__DYNAPP_NATIVE_HOST_CONFIG__") {
		t.Fatalf("control ready = %#v", ready)
	}
	writeNative(t, control, map[string]any{"type": "native-host-review", "id": "r1"})
	request := readNative(t, control)
	if request["kind"] != "review" {
		t.Fatalf("review request = %#v", request)
	}
	groups, _ := request["groups"].([]any)
	granted := map[string]bool{}
	for _, group := range groups {
		object := group.(map[string]any)
		granted[object["id"].(string)] = object["granted"].(bool)
	}
	if !granted["filesystem"] || granted["execute"] {
		t.Fatalf("review groups do not reflect the current grant: %#v", groups)
	}
	writeNative(t, control, map[string]any{"type": "native-host-permission-decision", "requestId": request["requestId"], "capabilities": []string{"fs.exec"}})
	result := readNative(t, control)
	if result["type"] != "native-host-review-result" || result["id"] != "r1" || result["changed"] != true {
		t.Fatalf("review result = %#v", result)
	}
	if closed := readNative(t, page); closed["type"] != "native-host-close" || closed["code"] != float64(websocket.StatusServiceRestart) {
		t.Fatalf("page socket after review = %#v", closed)
	}
	stored, _ := agent.nativeApp(app.StoreID)
	if strings.Join(stored.Capabilities, ",") != "fs.exec" {
		t.Fatalf("stored grant = %#v", stored.Capabilities)
	}
}

func TestNativeGrantsAreManagedThroughThePermissionsService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	agent, _, _, authority := newAuthorityAgent(t, ctx)
	app := notesNativeApp()
	app.ApprovedAt = time.Now().UnixMilli()
	app.Capabilities = []string{"fs.readText"}
	app.Declared = []string{"fs.exec", "fs.readText"}
	agent.upsertNativeApp(app)
	list := permissionsRPC(t, ctx, authority, "list", "list")
	var grant map[string]any
	for _, item := range list["grants"].([]any) {
		if object := item.(map[string]any); object["keyId"] == "native:amit-bet/notes" {
			grant = object
		}
	}
	if grant == nil || grant["origin"] != nativeNotesOrigin || grant["native"] != true {
		t.Fatalf("native grant missing from %#v", list["grants"])
	}
	permissionsRPC(t, ctx, authority, "update", "update", map[string]any{"keyId": grant["keyId"], "origin": grant["origin"], "storeId": grant["storeId"], "capabilities": []string{"fs.exec", "secrets.manage"}})
	if stored, _ := agent.nativeApp(app.StoreID); strings.Join(stored.Capabilities, ",") != "fs.exec" {
		t.Fatalf("updated native grant = %#v", stored.Capabilities)
	}
	permissionsRPC(t, ctx, authority, "revoke", "revoke", map[string]any{"keyId": grant["keyId"], "origin": grant["origin"], "storeId": grant["storeId"]})
	if stored, ok := agent.nativeApp(app.StoreID); !ok || stored.ApprovedAt != 0 || len(stored.Capabilities) != 0 {
		t.Fatalf("revoked native grant = %#v (installed %v)", stored, ok)
	}
}

func TestNativeAppsRPCIsLimitedToThePermissionAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, _, authority := newAuthorityAgent(t, ctx)
	sendRequest(t, ctx, authority, map[string]any{"type": "rpc", "id": "support", "service": "apps", "method": "nativeSupport"})
	reply := receive(t, ctx, authority)
	result, _ := reply["result"].(map[string]any)
	if reply["type"] != "rpc-result" || result["platform"] == nil {
		t.Fatalf("nativeSupport reply = %#v", reply)
	}

	server := &Server{}
	request := message{Service: "apps", Method: "listNative", auth: &socketAuthentication{capabilities: []string{"apps.manage"}, ok: true}}
	if _, err := server.handleAppsRPC(ctx, request); err == nil || !strings.Contains(err.Error(), permissionsManage) {
		t.Fatalf("listNative without permissions.manage = %v", err)
	}
}

func TestNativeCatalogInstallIsTheAuthority(t *testing.T) {
	agent, _ := nativeTestAgent(t, NativeApp{
		StoreID: "amit-bet/dyner", Name: "Dyner", Origin: "https://amitbet-dyner.dynapp.io", Path: "/Dyner.app",
		Authority: true, ApprovedAt: 1, Capabilities: []string{"apps.manage", permissionsManage},
	})
	host := dialNative(t, agent, "amit-bet/dyner", "app")
	ready := readNative(t, host)
	capabilities := anyStrings(ready["capabilities"])
	if ready["type"] != "native-host-ready" || !stringSliceHas(capabilities, permissionsManage) || !stringSliceHas(capabilities, "apps.manage") {
		t.Fatalf("catalog ready = %#v", ready)
	}
}

func TestNativePermissionGroupsCarryReasonsAndDefaults(t *testing.T) {
	groups := nativePermissionGroups([]string{"fs.readText", "secrets.manage"}, []string{"fs.readText"}, map[string]string{"secrets.manage": "Stores the API key you enter so syncing keeps working."})
	byID := map[string]map[string]any{}
	for _, group := range groups {
		byID[group["id"].(string)] = group
	}
	if byID["secrets"]["suggested"] != false || byID["secrets"]["danger"] != "very-high" {
		t.Fatalf("very-high group must start unchecked: %#v", byID["secrets"])
	}
	if reasons := byID["secrets"]["reasons"].([]string); len(reasons) != 1 {
		t.Fatalf("reasons = %#v", reasons)
	}
	if byID["filesystem"]["granted"] != true {
		t.Fatalf("filesystem group = %#v", byID["filesystem"])
	}
}

// A config.json zero-filled by an interrupted write (seen on Windows after an
// antivirus killed the service) must not stop the agent from starting.
func TestUnreadableConfigIsSetAsideInsteadOfBlockingStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig with a zero-filled file: %v", err)
	}
	if config.SchemaVersion != ConfigSchemaVersion || len(config.BrowserIdentities) != 0 {
		t.Fatalf("config = %#v", config)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the damaged file is still in place: %v", err)
	}
	damaged, _ := filepath.Glob(path + ".damaged-*")
	if len(damaged) != 1 {
		t.Fatalf("damaged copies = %v", damaged)
	}
	// A newer agent's config is valid JSON and must still fail loudly rather
	// than be discarded.
	if err := os.WriteFile(path, []byte(`{"schemaVersion":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("a newer schema version was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a newer config was moved aside: %v", err)
	}
}

func TestNativeOpenFilesBoundToGrantedApp(t *testing.T) {
	server := &Server{StateDir: t.TempDir(), Config: Config{NativeApps: NativeAppList{
		{StoreID: "owner/allowed", Declared: []string{"externalOpen.files"}, Capabilities: []string{"externalOpen.files"}},
		{StoreID: "owner/denied", Declared: []string{"externalOpen.files"}},
	}}}
	path := filepath.Join(t.TempDir(), "Unicode-לידור.xlsx")
	if err := os.WriteFile(path, []byte("xlsx bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.queueNativeOpenFiles("owner/denied", []string{path}); err == nil {
		t.Fatal("ungranted app accepted a file")
	}
	if err := server.queueNativeOpenFiles("owner/allowed", []string{path, "relative.xlsx"}); err == nil {
		t.Fatal("relative path accepted")
	}
	if err := server.queueNativeOpenFiles("owner/allowed", []string{path}); err != nil {
		t.Fatal(err)
	}
	wrong, err := server.handleExternalOpenRPC(context.Background(), message{Method: "takeData", AppID: "owner/denied"})
	if err != nil || len(wrong.([]map[string]any)) != 0 {
		t.Fatalf("wrong app received files: %v %v", wrong, err)
	}
	records, err := server.handleExternalOpenRPC(context.Background(), message{Method: "takeData", AppID: "owner/allowed"})
	if err != nil || len(records.([]map[string]any)) != 1 || records.([]map[string]any)[0]["path"] != path {
		t.Fatalf("open records: %v %v", records, err)
	}
}
