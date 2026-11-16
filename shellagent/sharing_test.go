package shellagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

var guestCapabilities = []string{"net.protocol.http", "net.protocol.https", "net.tcp.connect", "net.udp.connect"}

func TestRelayGrantForSharedKeyBinding(t *testing.T) {
	identities := []BrowserIdentity{
		{KeyID: "authority", Origin: "https://dynapp.io", Authority: true, Capabilities: []string{"permissions.manage"}},
		{KeyID: "owner-key", Origin: "https://dynapp.io", UserID: "u_owner", Capabilities: []string{"fs.remote", "net.protocol.https"}},
		{KeyID: "member-key", Origin: "https://dynapp.io", UserID: "u_member", Capabilities: []string{"fs.remote", "clipboard.write"}},
		{KeyID: "legacy-key", Origin: "https://dynapp.io", Capabilities: []string{"fs.remote", "fs.exec"}},
		{KeyID: "guest-key", Origin: "https://dynapp.io", UserID: "u_guest", Shared: true, Capabilities: guestCapabilities, Targets: []string{"192.168.1.81:8096"}},
	}
	resolve := func(string) (resolvedApp, bool) { return resolvedApp{}, false }
	owner := sessionSubject{UserID: "u_owner"}
	member := sessionSubject{UserID: "u_member"}
	guest := sessionSubject{UserID: "u_guest", Shared: true}
	unknown := sessionSubject{}
	fallback := []string{"fs.remote"} // owner ∩ member ∩ legacy; never the guest's
	cases := []struct {
		name         string
		key          string
		subject      sessionSubject
		wantKey      string
		wantCaps     []string
		wantScoped   bool
		wantTargets  []string
		wantStoredOK bool
	}{
		{name: "owner key, owner subject", key: "owner-key", subject: owner, wantKey: "owner-key", wantCaps: []string{"fs.remote", "net.protocol.https"}},
		{name: "owner key, old Dyner (no subject)", key: "owner-key", subject: unknown, wantKey: "owner-key", wantCaps: []string{"fs.remote", "net.protocol.https"}},
		{name: "owner key presented by member", key: "owner-key", subject: member, wantCaps: fallback},
		{name: "owner key presented by grantee", key: "owner-key", subject: guest, wantScoped: true},
		{name: "legacy key, member subject", key: "legacy-key", subject: member, wantKey: "legacy-key", wantCaps: []string{"fs.exec", "fs.remote"}},
		{name: "legacy key presented by grantee", key: "legacy-key", subject: guest, wantScoped: true},
		{name: "guest key, guest subject", key: "guest-key", subject: guest, wantKey: "guest-key", wantCaps: guestCapabilities, wantScoped: true, wantTargets: []string{"192.168.1.81:8096"}},
		{name: "guest key, guest subject via ICE (shared unknown)", key: "guest-key", subject: sessionSubject{UserID: "u_guest"}, wantKey: "guest-key", wantCaps: guestCapabilities, wantScoped: true, wantTargets: []string{"192.168.1.81:8096"}},
		{name: "guest key without subject", key: "guest-key", subject: unknown, wantCaps: fallback},
		{name: "guest key presented by member", key: "guest-key", subject: member, wantCaps: fallback},
		{name: "no key, grantee", subject: guest, wantScoped: true},
		{name: "no key, unknown", subject: unknown, wantCaps: fallback},
		{name: "unknown key, grantee", key: "nobody", subject: guest, wantScoped: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			auth := relayGrantFor(identities, message{KeyID: test.key}, test.subject, resolve)
			if !auth.ok || auth.keyID != test.wantKey || auth.scoped != test.wantScoped {
				t.Fatalf("auth = %#v", auth)
			}
			if !reflect.DeepEqual(normalizePermissionSet(auth.capabilities), normalizePermissionSet(test.wantCaps)) {
				t.Fatalf("capabilities = %v, want %v", auth.capabilities, test.wantCaps)
			}
			if !reflect.DeepEqual(normalizeTargets(auth.targets), normalizeTargets(test.wantTargets)) {
				t.Fatalf("targets = %v, want %v", auth.targets, test.wantTargets)
			}
		})
	}
}

func TestRelayGrantFallbackIntersectionDoesNotResetWhenEmpty(t *testing.T) {
	identities := []BrowserIdentity{
		{KeyID: "a", Origin: "https://dynapp.io", Capabilities: []string{"fs.remote"}},
		{KeyID: "b", Origin: "https://dynapp.io", Capabilities: []string{"clipboard.write"}},
		{KeyID: "c", Origin: "https://dynapp.io", Capabilities: []string{"fs.exec"}},
	}
	auth := relayGrantFor(identities, message{}, sessionSubject{}, func(string) (resolvedApp, bool) { return resolvedApp{}, false })
	if len(auth.capabilities) != 0 {
		t.Fatalf("empty intersection grew back: %v", auth.capabilities)
	}
}

func TestSyncParsesSharedIdentities(t *testing.T) {
	owner := testBrowserIdentity(t, "https://dynapp.io")
	guest := testBrowserIdentity(t, "https://dynapp.io")
	nameless := testBrowserIdentity(t, "https://dynapp.io")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{
			"ownerId": "u_owner",
			"browserIdentities": []map[string]any{{
				"keyId": owner.KeyID, "publicKeyJwk": owner.PublicKeyJWK, "origin": owner.Origin, "capabilities": []string{"fs.remote"}, "userId": "u_owner",
				// A browser identity can never be shared, whatever Dyner says.
				"shared": true, "targets": []string{"10.0.0.1:22"},
			}},
			"sharedIdentities": []map[string]any{
				{
					"keyId": guest.KeyID, "publicKeyJwk": guest.PublicKeyJWK, "origin": guest.Origin, "userId": "u_guest",
					"capabilities": []string{"net.protocol.http", "net.tcp.connect", "fs.remote"},
					"targets":      []string{"NAS.local:8096", "192.168.1.81:80", "not a target", "[FE80::1]:443", "nas.local:8096"},
					"authority":    true, "approvedAt": 1, "storeId": "owner/app", "connect": map[string]any{"http": []string{"http://x/"}},
				},
				{"keyId": nameless.KeyID, "publicKeyJwk": nameless.PublicKeyJWK, "origin": nameless.Origin, "capabilities": guestCapabilities, "targets": []string{"1.2.3.4:80"}},
				{"keyId": "broken", "publicKeyJwk": map[string]any{}, "origin": "https://dynapp.io", "userId": "u_x"},
			},
		})
	}))
	defer server.Close()
	state, err := SyncRemoteEnvironmentState(context.Background(), server.Client(), Config{
		SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.OwnerID != "u_owner" || len(state.BrowserIdentities) != 2 || state.SharedIdentities != nil {
		t.Fatalf("state = %#v", state)
	}
	browser, shared := state.BrowserIdentities[0], state.BrowserIdentities[1]
	if browser.Shared || browser.Targets != nil || browser.UserID != "u_owner" {
		t.Fatalf("browser identity = %#v", browser)
	}
	if !shared.Shared || shared.UserID != "u_guest" || shared.Authority || shared.ApprovedAt != 0 || shared.StoreID != "" || shared.Connect != nil {
		t.Fatalf("shared identity = %#v", shared)
	}
	if !reflect.DeepEqual(shared.Capabilities, []string{"net.protocol.http", "net.tcp.connect"}) {
		t.Fatalf("shared capabilities = %v", shared.Capabilities)
	}
	if !reflect.DeepEqual(shared.Targets, []string{"192.168.1.81:80", "[fe80::1]:443", "nas.local:8096"}) {
		t.Fatalf("shared targets = %v", shared.Targets)
	}
	// Shared identities are replaced on every sync and never kept as local
	// approvals, even if one somehow carries an approval time.
	stale := shared
	stale.ApprovedAt = 5
	if merged := mergeSyncedIdentities([]BrowserIdentity{stale}, nil, nil); len(merged) != 0 {
		t.Fatalf("shared identity survived a sync: %#v", merged)
	}
	if merged := mergeSyncedIdentities(nil, state.BrowserIdentities, nil); len(merged) != 2 {
		t.Fatalf("merge dropped synced identities: %#v", merged)
	}
}

func TestRelayTicketAndICESessionParseSharingFields(t *testing.T) {
	var relay RelayConnection
	if err := json.Unmarshal([]byte(`{
		"acceptedAuthTokenHashes": ["AAAA"],
		"ownerId": "u_owner",
		"acceptedAccess": [{"authTokenHash": "AAAA", "userId": "u_owner", "shared": false}, {"authTokenHash": "BBBB", "userId": "u_guest", "shared": true}],
		"sharedIdentities": []
	}`), &relay); err != nil {
		t.Fatal(err)
	}
	accepted, subjects := relayAcceptedAccess(relay)
	if relay.OwnerID != "u_owner" || !accepted["aaaa"] || !accepted["bbbb"] || len(accepted) != 2 {
		t.Fatalf("accepted = %v", accepted)
	}
	if subjects["aaaa"] != (sessionSubject{UserID: "u_owner"}) || subjects["bbbb"] != (sessionSubject{UserID: "u_guest", Shared: true}) {
		t.Fatalf("subjects = %v", subjects)
	}
	// Old Dyner: no acceptedAccess, every hash has an unknown subject.
	accepted, subjects = relayAcceptedAccess(RelayConnection{AcceptedAuthTokenHashes: []string{"CCCC"}})
	if !accepted["cccc"] || len(subjects) != 0 {
		t.Fatalf("legacy accepted = %v %v", accepted, subjects)
	}
	var session iceSession
	if err := json.Unmarshal([]byte(`{"id":"ice_1","browserUserId":"u_guest"}`), &session); err != nil || session.BrowserUserID != "u_guest" {
		t.Fatalf("ice session = %#v, %v", session, err)
	}
}

func TestSharedScopeErrorTable(t *testing.T) {
	auth := &socketAuthentication{scoped: true, targets: []string{"192.168.1.81:8096", "nas.local:80", "[fe80::1]:443"}}
	rpc := func(method, target string) message {
		return message{Type: "rpc", Service: "http", Method: method, Args: []any{map[string]any{"url": target}}}
	}
	netOpen := func(protocol, host string, port int) message {
		request := message{Type: "net", Action: "open", ProtocolName: protocol}
		request.Target.Host, request.Target.Port = host, port
		return request
	}
	cases := []struct {
		name    string
		request message
		allowed bool
	}{
		{"ping", message{Type: "ping"}, true},
		{"net-data", message{Type: "net-data"}, true},
		{"net-close", message{Type: "net-close"}, true},
		{"channel", message{Type: "channel"}, true},
		{"tcp target", netOpen("tcp", "192.168.1.81", 8096), true},
		{"udp target, host case", netOpen("udp", "NAS.local", 80), true},
		{"tcp ipv6 target", netOpen("tcp", "[FE80::1]", 443), true},
		{"tcp wrong port", netOpen("tcp", "192.168.1.81", 22), false},
		{"tcp wrong host", netOpen("tcp", "192.168.1.82", 8096), false},
		{"rdp target", netOpen("rdp", "192.168.1.81", 8096), false},
		{"net data action", message{Type: "net", Action: "data"}, true},
		{"http default port", rpc("request", "http://nas.local/library"), true},
		{"https default port", rpc("request", "https://[fe80::1]/"), true},
		{"http explicit port", rpc("request", "http://192.168.1.81:8096/web"), true},
		{"http wrong port", rpc("request", "http://192.168.1.81/web"), false},
		{"https on http target", rpc("request", "https://nas.local/"), false},
		{"http download", rpc("download", "http://nas.local/"), false},
		{"fs", message{Type: "fs", Method: "readText"}, false},
		{"exec", message{Type: "exec", Command: "ls"}, false},
		{"process", message{Type: "process"}, false},
		{"rpc clipboard", message{Type: "rpc", Service: "clipboard", Method: "readText"}, false},
		{"rpc ftp", message{Type: "rpc", Service: "ftp"}, false},
		{"unknown", message{Type: "whatever"}, false},
	}
	for _, test := range cases {
		got := sharedScopeError(auth, test.request)
		if (got == "") != test.allowed || (!test.allowed && got != errNotShared) {
			t.Errorf("%s: sharedScopeError = %q, allowed %v", test.name, got, test.allowed)
		}
	}
	if sharedScopeError(&socketAuthentication{}, message{Type: "fs"}) != "" {
		t.Fatal("unscoped sockets must not be affected")
	}
	if sharedScopeError(&socketAuthentication{scoped: true}, netOpen("tcp", "192.168.1.81", 8096)) != errNotShared {
		t.Fatal("a scope without targets reaches nothing")
	}
}

func TestSharedIdentityStillCovers(t *testing.T) {
	identities := []BrowserIdentity{{KeyID: "k", Shared: true, Targets: []string{"a:1", "b:2"}}}
	if !sharedIdentityStillCovers(identities, "k", []string{"a:1"}) || sharedIdentityStillCovers(identities, "k", []string{"c:3"}) || sharedIdentityStillCovers(nil, "k", []string{"a:1"}) {
		t.Fatal("coverage check is wrong")
	}
}

// scopedDispatchHarness serves one relay-style socket for a grantee through
// the shared session core and lets the test exchange frames with it.
type scopedDispatchHarness struct {
	t       *testing.T
	carrier *memoryCarrier
	read    int
}

func startScopedDispatch(t *testing.T, agent *Server, authenticate func(message) socketAuthentication) *scopedDispatchHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	carrier := newMemoryCarrier()
	go agent.serveAuthenticatedSocket(ctx, carrier, authenticate, carrierCapabilities{}, nil)
	harness := &scopedDispatchHarness{t: t, carrier: carrier}
	harness.send(map[string]any{"type": "hello", "protocol": ProtocolVersion, "keyId": "ignored", "app": map[string]any{"storeId": "owner/app"}})
	if hello := harness.next(); hello["type"] != "hello" || hello["ok"] != true {
		t.Fatalf("hello = %#v", hello)
	}
	return harness
}

func (h *scopedDispatchHarness) send(value any) {
	h.t.Helper()
	data, _ := json.Marshal(value)
	h.carrier.incoming <- dataChannelMessage{kind: websocket.MessageText, data: data}
}

func (h *scopedDispatchHarness) next() map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.carrier.mu.Lock()
		if h.read < len(h.carrier.written) {
			data := h.carrier.written[h.read]
			h.read++
			h.carrier.mu.Unlock()
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				h.t.Fatalf("frame %q: %v", data, err)
			}
			return frame
		}
		h.carrier.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("no frame from the agent")
	return nil
}

func (h *scopedDispatchHarness) roundTrip(value any) map[string]any {
	h.t.Helper()
	h.send(value)
	return h.next()
}

func hostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(parsed.Port())
	return parsed.Hostname(), port
}

func TestScopedSocketDispatch(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("other")) }))
	defer other.Close()
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("shared"))
	}))
	defer shared.Close()
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
			// Keep it open so no asynchronous net-close interleaves.
			defer connection.Close()
		}
	}()
	tcpPort := listener.Addr().(*net.TCPAddr).Port
	sharedHost, sharedPort := hostPort(t, shared.URL)

	guest := testBrowserIdentity(t, "https://dynapp.io")
	guest.UserID, guest.Shared, guest.Capabilities = "u_guest", true, guestCapabilities
	guest.Targets = []string{net.JoinHostPort(sharedHost, strconv.Itoa(sharedPort)), net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort))}
	agent := &Server{Config: Config{SchemaVersion: ConfigSchemaVersion, BrowserIdentities: []BrowserIdentity{guest}}}
	resolve := func(storeID string) (resolvedApp, bool) {
		// The app's connect list allows both servers; only the share limits
		// the grantee to the first one.
		return resolvedApp{StoreID: storeID, Name: "App", Declared: append([]string{"fs.remote", "fs.exec"}, guestCapabilities...), Connect: map[string][]string{
			"http": {shared.URL + "/", other.URL + "/"},
		}}, true
	}
	harness := startScopedDispatch(t, agent, func(hello message) socketAuthentication {
		hello.KeyID = guest.KeyID
		return relayGrantFor(agent.Config.BrowserIdentities, hello, sessionSubject{UserID: "u_guest", Shared: true}, resolve)
	})

	if frame := harness.roundTrip(map[string]any{"type": "ping", "id": "p"}); frame["type"] != "pong" {
		t.Fatalf("ping = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "fs", "id": "f", "method": "readText", "args": []any{"/etc/hosts"}}); frame["type"] != "fs-error" || frame["error"] != errNotShared {
		t.Fatalf("fs = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "exec", "id": "e", "command": "id"}); frame["type"] != "exec-error" || frame["error"] != errNotShared {
		t.Fatalf("exec = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "rpc", "id": "c", "service": "clipboard", "method": "readText"}); frame["type"] != "rpc-error" || frame["error"] != errNotShared {
		t.Fatalf("clipboard = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "net", "id": "n1", "protocol": "tcp", "action": "open", "target": map[string]any{"host": "127.0.0.1", "port": tcpPort}}); frame["type"] != "net-result" {
		t.Fatalf("tcp target = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "net", "id": "n2", "protocol": "tcp", "action": "open", "target": map[string]any{"host": "127.0.0.1", "port": tcpPort + 1}}); frame["type"] != "net-error" || frame["error"] != errNotShared {
		t.Fatalf("tcp wrong port = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "rpc", "id": "h1", "service": "http", "method": "request", "args": []any{map[string]any{"url": shared.URL + "/"}}}); frame["type"] != "rpc-result" || frame["result"].(map[string]any)["text"] != "shared" {
		t.Fatalf("http target = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "rpc", "id": "h2", "service": "http", "method": "request", "args": []any{map[string]any{"url": other.URL + "/"}}}); frame["type"] != "rpc-error" || frame["error"] != errNotShared {
		t.Fatalf("http non-target = %#v", frame)
	}
	if frame := harness.roundTrip(map[string]any{"type": "rpc", "id": "h3", "service": "http", "method": "request", "args": []any{map[string]any{"url": shared.URL + "/redirect"}}}); frame["type"] != "rpc-error" || !strings.Contains(fmt.Sprint(frame["error"]), errNotShared) {
		t.Fatalf("http redirect to non-target = %#v", frame)
	}
}

// A carrier that forgot to scope a socket whose key belongs only to shared
// identities still ends up scoped.
func TestSharedKeyIsScopedRegardlessOfCarrier(t *testing.T) {
	guest := testBrowserIdentity(t, "https://dynapp.io")
	guest.UserID, guest.Shared, guest.Capabilities, guest.Targets = "u_guest", true, guestCapabilities, []string{"192.168.1.81:8096"}
	agent := &Server{Config: Config{SchemaVersion: ConfigSchemaVersion, BrowserIdentities: []BrowserIdentity{guest}}}
	harness := startScopedDispatch(t, agent, func(message) socketAuthentication {
		return socketAuthentication{keyID: guest.KeyID, origin: guest.Origin, capabilities: []string{"fs.remote"}, stored: guest.Capabilities, ok: true}
	})
	if frame := harness.roundTrip(map[string]any{"type": "fs", "id": "f", "method": "readText", "args": []any{"/etc/hosts"}}); frame["type"] != "fs-error" || frame["error"] != errNotShared {
		t.Fatalf("fs on unscoped shared key = %#v", frame)
	}
}

// LAN/loopback: the key proves itself by signing the challenge; a shared
// identity's socket is scoped and the identity never becomes a local grant.
func TestDirectLocalSharedIdentityIsScoped(t *testing.T) {
	agent := newTestServer(t)
	browser := newTestBrowser(t)
	agent.Config = Config{SchemaVersion: ConfigSchemaVersion, BrowserIdentities: []BrowserIdentity{{
		KeyID: browser.keyID, PublicKeyJWK: browser.key, Origin: testDevelopmentOrigin, UserID: "u_guest", Shared: true,
		Capabilities: guestCapabilities, Targets: []string{"192.168.1.81:8096"},
	}}}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, hello := dialTestAgent(t, ctx, server, testDialOptions{Browser: browser})
	defer socket.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" {
		t.Fatalf("hello = %#v", hello)
	}
	sendRequest(t, ctx, socket, map[string]any{"type": "fs", "id": "f", "method": "readText", "args": []any{"/etc/hosts"}})
	if frame := receive(t, ctx, socket); frame["type"] != "fs-error" || frame["error"] != errNotShared {
		t.Fatalf("fs over LAN = %#v", frame)
	}
	sendRequest(t, ctx, socket, map[string]any{"type": "net", "id": "n", "protocol": "tcp", "action": "open", "target": map[string]any{"host": "127.0.0.1", "port": 9}})
	if frame := receive(t, ctx, socket); frame["type"] != "net-error" || frame["error"] != errNotShared {
		t.Fatalf("net over LAN = %#v", frame)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	for _, identity := range agent.Config.BrowserIdentities {
		if identity.ApprovedAt != 0 || !identity.Shared {
			t.Fatalf("shared identity became a local grant: %#v", identity)
		}
	}
}

func TestScopedFilesystemChannelIsDenied(t *testing.T) {
	carrier := newMemoryCarrier()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data, _ := json.Marshal(map[string]any{"type": "fs", "id": "s", "method": "readChunk"})
	carrier.incoming <- dataChannelMessage{kind: websocket.MessageText, data: data}
	(&Server{}).handleFilesystemChannel(ctx, carrier, message{ID: "s"}, socketAuthentication{scoped: true, capabilities: []string{"fs.remote"}, ok: true})
	carrier.mu.Lock()
	defer carrier.mu.Unlock()
	if len(carrier.written) != 1 || !strings.Contains(string(carrier.written[0]), errNotShared) {
		t.Fatalf("filesystem channel = %q", carrier.written)
	}
}

func TestUpdateGrantRefusesSharedIdentity(t *testing.T) {
	guest := testBrowserIdentity(t, "https://dynapp.io")
	guest.Shared, guest.UserID = true, "u_guest"
	agent := &Server{Config: Config{SchemaVersion: ConfigSchemaVersion, BrowserIdentities: []BrowserIdentity{guest}}}
	if _, err := agent.updateGrant(map[string]any{"keyId": guest.KeyID, "origin": guest.Origin, "capabilities": []any{"fs.remote"}}); err == nil {
		t.Fatal("a shared identity must not be edited into a local grant")
	}
}

// ICE: the session's browserUserId is the subject. A grantee's key that only
// arrives with the refresh sync is bound to that user and scoped.
func TestAuthenticateICEHelloBindsSharedKeyToBrowserUser(t *testing.T) {
	guest := testBrowserIdentity(t, "https://dynapp.io")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{"sharedIdentities": []map[string]any{{
			"keyId": guest.KeyID, "publicKeyJwk": guest.PublicKeyJWK, "origin": guest.Origin, "userId": "u_guest",
			"capabilities": guestCapabilities, "targets": []string{"192.168.1.81:8096"},
		}}})
	}))
	defer server.Close()
	config := Config{
		SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL, EnvironmentID: "env_test",
		DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
	}
	hello := message{Type: "hello", Protocol: json.RawMessage("2"), KeyID: guest.KeyID}
	agent := &Server{}
	if grant := agent.authenticateICEHello(context.Background(), config, hello, sessionSubject{UserID: "u_guest"}); !grant.ok || !grant.scoped || !reflect.DeepEqual(grant.targets, []string{"192.168.1.81:8096"}) {
		t.Fatalf("grantee ICE grant = %#v", grant)
	}
	for _, subject := range []sessionSubject{{}, {UserID: "u_owner"}} {
		if grant := agent.authenticateICEHello(context.Background(), config, hello, subject); grant.ok {
			t.Fatalf("shared key accepted for subject %#v: %#v", subject, grant)
		}
	}
}
