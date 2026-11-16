package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const testServerAPI = "amit-bet/formfuel-journal"

// testDynappServer implements the Phase 3a server protocol in-test.
type testDynappServer struct {
	*httptest.Server
	key      *ecdsa.PrivateKey
	jwk      BrowserJWK
	id       string
	provides []serverProvide
	// signWith, signAddress and claimID let a test break the proof.
	signWith    *ecdsa.PrivateKey
	signAddress string
	claimID     string
	challenges  int
	redirectTo  string
}

func newTestKey(t *testing.T) (*ecdsa.PrivateKey, BrowserJWK) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(b []byte) string {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	return key, BrowserJWK{KTY: "EC", CRV: "P-256", X: pad(key.X.Bytes()), Y: pad(key.Y.Bytes())}
}

func signP1363(t *testing.T, key *ecdsa.PrivateKey, message []byte) string {
	t.Helper()
	hash := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return base64.RawURLEncoding.EncodeToString(signature)
}

func newTestDynappServer(t *testing.T, apis ...string) *testDynappServer {
	t.Helper()
	if len(apis) == 0 {
		apis = []string{testServerAPI}
	}
	server := &testDynappServer{}
	server.key, server.jwk = newTestKey(t)
	server.id = jwkThumbprint(server.jwk)
	for _, api := range apis {
		server.provides = append(server.provides, serverProvide{API: api, V: 1})
	}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case serverDescriptorPath:
			id := server.id
			if server.claimID != "" {
				id = server.claimID
			}
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"v": 1, "instanceId": id, "publicKeyJwk": server.jwk, "name": "Journal", "version": "0.8.0",
				"provides": server.provides, "apps": []string{"amit-bet/formfuel"}, "issuer": "https://dynapp.io",
			})
		case serverChallengePath:
			var body struct{ Nonce, Address string }
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			server.challenges++
			key := server.key
			if server.signWith != nil {
				key = server.signWith
			}
			address := body.Address
			if server.signAddress != "" {
				address = server.signAddress
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"instanceId": server.id, "signature": signP1363(t, key, serverChallengeMessage(body.Nonce, address))})
		case "/redirect-away":
			http.Redirect(w, r, server.redirectTo, http.StatusFound)
		case "/redirect-home":
			http.Redirect(w, r, "/data", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "server:"+r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *testDynappServer) address(t *testing.T) string {
	t.Helper()
	parsed, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

func TestJWKThumbprintRFC7638(t *testing.T) {
	// RFC 7638 canonical form: required members only, lexicographic order.
	key := BrowserJWK{KTY: "EC", CRV: "P-256", X: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", Y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU","y":"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}`))
	if got, want := jwkThumbprint(key), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("thumbprint = %s, want %s", got, want)
	}
}

func TestServerChallengeVerification(t *testing.T) {
	ctx := context.Background()
	t.Run("good", func(t *testing.T) {
		server := newTestDynappServer(t)
		var registry serverRegistry
		descriptor, err := registry.verifyAt(ctx, "http", server.address(t), server.id)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.InstanceID != server.id || descriptor.Provides[0].API != testServerAPI {
			t.Fatalf("descriptor = %#v", descriptor)
		}
	})
	t.Run("wrong address", func(t *testing.T) {
		server := newTestDynappServer(t)
		server.signAddress = "192.168.1.9:9018"
		var registry serverRegistry
		if _, err := registry.verifyAt(ctx, "http", server.address(t), ""); err == nil {
			t.Fatal("a proof for another address must fail")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		server := newTestDynappServer(t)
		server.signWith, _ = newTestKey(t)
		var registry serverRegistry
		if _, err := registry.verifyAt(ctx, "http", server.address(t), ""); err == nil {
			t.Fatal("a signature from another key must fail")
		}
	})
	t.Run("instance id is not the thumbprint", func(t *testing.T) {
		server := newTestDynappServer(t)
		_, other := newTestKey(t)
		server.claimID = jwkThumbprint(other)
		var registry serverRegistry
		if _, err := registry.verifyAt(ctx, "http", server.address(t), ""); err == nil || !strings.Contains(err.Error(), "thumbprint") {
			t.Fatalf("thumbprint mismatch error = %v", err)
		}
	})
	t.Run("unexpected instance id", func(t *testing.T) {
		server := newTestDynappServer(t)
		_, other := newTestKey(t)
		var registry serverRegistry
		if _, err := registry.verifyAt(ctx, "http", server.address(t), jwkThumbprint(other)); err == nil {
			t.Fatal("verify must reject a different instance id")
		}
		if server.challenges != 0 {
			t.Fatal("no challenge should be sent to the wrong instance")
		}
	})
}

func TestServersAnnounce(t *testing.T) {
	server := newTestDynappServer(t)
	_, port, _ := net.SplitHostPort(server.address(t))
	agent := &Server{}
	announce := func(remote string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9011/v1/servers/announce", strings.NewReader(`{"port":`+port+`,"scheme":"http"}`))
		request.RemoteAddr = remote
		request.Header.Set("Content-Type", "application/json")
		if mutate != nil {
			mutate(request)
		}
		recorder := httptest.NewRecorder()
		agent.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	if got := announce("192.168.1.20:5000", nil).Code; got != http.StatusForbidden {
		t.Fatalf("LAN announce status = %d", got)
	}
	if got := announce("127.0.0.1:5000", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }).Code; got != http.StatusForbidden {
		t.Fatalf("browser announce status = %d", got)
	}
	if got := announce("127.0.0.1:5000", func(r *http.Request) { r.Host = "evil.example:9011" }).Code; got != http.StatusForbidden {
		t.Fatalf("rebound host announce status = %d", got)
	}
	if len(agent.servers.list()) != 0 || server.challenges != 0 {
		t.Fatal("rejected announcements must not reach the server")
	}
	response := announce("127.0.0.1:5000", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), server.id) {
		t.Fatalf("announce = %d %s", response.Code, response.Body)
	}
	entries := agent.servers.list()
	if len(entries) != 1 || entries[0].Source != "announce" || entries[0].Address != "127.0.0.1:"+port || entries[0].InstanceID != server.id {
		t.Fatalf("entries = %#v", entries)
	}
	// A server that cannot prove its key is not recorded.
	liar := newTestDynappServer(t)
	liar.signWith, _ = newTestKey(t)
	_, liarPort, _ := net.SplitHostPort(liar.address(t))
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9011/v1/servers/announce", strings.NewReader(`{"port":`+liarPort+`}`))
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	agent.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway || len(agent.servers.list()) != 1 {
		t.Fatalf("unproven announce = %d, entries %d", recorder.Code, len(agent.servers.list()))
	}
}

func TestServerRegistryExpiryAndAddressChange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	registry := serverRegistry{now: func() time.Time { return now }}
	_, jwk := newTestKey(t)
	descriptor := serverDescriptor{V: 1, InstanceID: jwkThumbprint(jwk), PublicKeyJWK: jwk, Provides: []serverProvide{{API: testServerAPI, V: 1}}}
	apis := []ServerAPI{{API: testServerAPI, Min: 1, Max: 1}}
	registry.recordEntry("192.168.1.5:9018", "http", "mdns", descriptor)
	registry.recordEntry("192.168.1.6:9018", "http", "probe", descriptor)
	if entries := registry.list(); len(entries) != 1 || entries[0].Address != "192.168.1.6:9018" {
		t.Fatalf("entries = %#v", entries)
	}
	if !registry.recentlyVerified("192.168.1.6:9018", "http", apis) || registry.recentlyVerified("192.168.1.6:9018", "https", apis) {
		t.Fatal("verify cache is wrong")
	}
	now = now.Add(61 * time.Second)
	if registry.recentlyVerified("192.168.1.6:9018", "http", apis) {
		t.Fatal("verification must expire after 60 s")
	}
	if len(registry.list()) != 1 {
		t.Fatal("entry must survive until 10 minutes")
	}
	now = now.Add(10 * time.Minute)
	if len(registry.list()) != 0 {
		t.Fatal("entry must expire after 10 minutes")
	}
}

func scopedServersAuth(targets ...string) *socketAuthentication {
	return &socketAuthentication{ok: true, scoped: true, targets: normalizeTargets(targets), capabilities: sharedCapabilityCeiling,
		serverAPIs: []ServerAPI{{API: testServerAPI, Min: 1, Max: 1}}}
}

func TestServersRPCSocketGating(t *testing.T) {
	server := newTestDynappServer(t)
	address := server.address(t)
	agent := &Server{}
	call := func(auth *socketAuthentication, method string, args map[string]any) (any, error) {
		return agent.handleServersRPC(context.Background(), message{Type: "rpc", Service: "servers", Method: method, Args: []any{args}, auth: auth})
	}
	scoped := scopedServersAuth(address)
	for _, method := range []string{"list", "discover"} {
		if _, err := call(scoped, method, map[string]any{}); err == nil || err.Error() != errNotShared {
			t.Fatalf("scoped %s error = %v", method, err)
		}
		request := message{Type: "rpc", Service: "servers", Method: method, Args: []any{map[string]any{}}}
		if sharedScopeError(scoped, request) != errNotShared {
			t.Fatalf("dispatch must refuse scoped %s", method)
		}
	}
	verify := map[string]any{"address": address, "instanceId": server.id}
	if sharedScopeError(scoped, message{Type: "rpc", Service: "servers", Method: "verify", Args: []any{verify}}) != "" {
		t.Fatal("dispatch must let a grantee verify its target")
	}
	result, err := call(scoped, "verify", verify)
	if err != nil || result.(map[string]any)["ok"] != true {
		t.Fatalf("scoped verify of target = %#v, %v", result, err)
	}
	other := map[string]any{"address": "192.168.1.200:9018", "instanceId": server.id}
	if sharedScopeError(scoped, message{Type: "rpc", Service: "servers", Method: "verify", Args: []any{other}}) != errNotShared {
		t.Fatal("dispatch must refuse a grantee verify outside its targets")
	}
	if _, err := call(scoped, "verify", other); err == nil || err.Error() != errNotShared {
		t.Fatalf("scoped verify off target error = %v", err)
	}
	// A grantee whose app does not declare servers.connect gets nothing.
	undeclared := scopedServersAuth(address)
	undeclared.serverAPIs = nil
	if _, err := call(undeclared, "verify", verify); err == nil {
		t.Fatal("verify needs a servers.connect declaration")
	}
	// Owners and members list and discover; grantees never need the grant.
	owner := &socketAuthentication{ok: true, capabilities: []string{serversPermission}, serverAPIs: scoped.serverAPIs}
	if serversRPCCapability(message{Service: "servers", auth: owner}) != serversPermission || serversRPCCapability(message{Service: "servers", auth: scoped}) != "" {
		t.Fatal("servers capability mapping is wrong")
	}
	agent.servers.mdnsBrowse = func(context.Context) []serverCandidate { return nil }
	agent.servers.probeHosts = func() []net.IP { return []net.IP{net.ParseIP("127.0.0.1").To4()} }
	_, port, _ := net.SplitHostPort(address)
	result, err = call(owner, "discover", map[string]any{"ports": []any{json.Number(port)}})
	if err == nil {
		t.Fatal("ports must be JSON numbers")
	}
	var portNumber float64
	_ = json.Unmarshal([]byte(port), &portNumber)
	result, err = call(owner, "discover", map[string]any{"ports": []any{portNumber}})
	if err != nil {
		t.Fatal(err)
	}
	servers := result.(map[string]any)["servers"].([]serverEntry)
	if len(servers) != 1 || servers[0].InstanceID != server.id || servers[0].Source != "probe" {
		t.Fatalf("discovered = %#v", servers)
	}
	if _, err := call(owner, "discover", map[string]any{"ports": []any{1.0, 2.0, 3.0, 4.0, 5.0}}); err == nil {
		t.Fatal("more than four ports must be refused")
	}
	result, err = call(owner, "list", nil)
	if err != nil || len(result.(map[string]any)["servers"].([]serverEntry)) != 1 {
		t.Fatalf("owner list = %#v, %v", result, err)
	}
}

func TestScopedServersVerifyThroughDispatch(t *testing.T) {
	server := newTestDynappServer(t)
	address := server.address(t)
	agent := &Server{}
	harness := startScopedDispatch(t, agent, func(message) socketAuthentication { return *scopedServersAuth(address) })
	frame := harness.roundTrip(map[string]any{"type": "rpc", "id": "l", "service": "servers", "method": "list", "args": []any{map[string]any{}}})
	if frame["type"] != "rpc-error" || frame["error"] != errNotShared {
		t.Fatalf("scoped list = %#v", frame)
	}
	frame = harness.roundTrip(map[string]any{"type": "rpc", "id": "v", "service": "servers", "method": "verify", "args": []any{map[string]any{"address": address, "instanceId": server.id}}})
	if frame["type"] != "rpc-result" {
		t.Fatalf("scoped verify = %#v", frame)
	}
	// The verified server is now reachable over HTTP without a connect list.
	frame = harness.roundTrip(map[string]any{"type": "rpc", "id": "h", "service": "http", "method": "request", "args": []any{map[string]any{"url": server.URL + "/data"}}})
	if frame["type"] != "rpc-result" || frame["result"].(map[string]any)["text"] != "server:/data" {
		t.Fatalf("scoped server request = %#v", frame)
	}
}

func TestHTTPToVerifiedServers(t *testing.T) {
	server := newTestDynappServer(t)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "elsewhere") }))
	defer elsewhere.Close()
	server.redirectTo = elsewhere.URL + "/"
	address := server.address(t)
	agent := &Server{}
	apis := []ServerAPI{{API: testServerAPI, Min: 1, Max: 1}}
	owner := func(apis []ServerAPI) *socketAuthentication {
		return &socketAuthentication{ok: true, keyID: "paired", capabilities: []string{serversPermission, "net.protocol.http"}, connect: map[string][]string{}, serverAPIs: apis}
	}
	get := func(auth *socketAuthentication, path string) (map[string]any, error) {
		result, err := agent.handleHTTPRPCBody(context.Background(), message{Type: "rpc", Service: "http", Method: "request", Args: []any{map[string]any{"url": server.URL + path}}, auth: auth}, nil)
		if err != nil {
			return nil, err
		}
		return result.(map[string]any), nil
	}
	if _, err := get(owner(apis), "/data"); err == nil {
		t.Fatal("an unverified server must not be reachable outside the connect list")
	}
	if _, err := agent.handleServersRPC(context.Background(), message{Service: "servers", Method: "verify", Args: []any{map[string]any{"address": address, "instanceId": server.id}}, auth: owner(apis)}); err != nil {
		t.Fatal(err)
	}
	result, err := get(owner(apis), "/data")
	if err != nil || result["text"] != "server:/data" {
		t.Fatalf("verified server request = %#v, %v", result, err)
	}
	if got := agent.rpcCapabilityFor(message{Service: "http", Method: "request", Args: []any{map[string]any{"url": server.URL + "/data"}}, auth: owner(apis)}); got != "" {
		t.Fatalf("verified server request still needs %q", got)
	}
	if _, err := get(owner([]ServerAPI{{API: "amit-bet/other", Min: 1}}), "/data"); err == nil {
		t.Fatal("a server that does not provide the declared API must be refused")
	}
	if _, err := get(owner([]ServerAPI{{API: testServerAPI, Min: 2, Max: 3}}), "/data"); err == nil {
		t.Fatal("a server outside the declared version range must be refused")
	}
	ungranted := owner(apis)
	ungranted.capabilities = []string{"net.protocol.http"}
	if _, err := get(ungranted, "/data"); err == nil {
		t.Fatal("an owner socket without servers.connect must be refused")
	}
	if _, err := get(owner(apis), "/redirect-away"); err == nil || !strings.Contains(err.Error(), "verified server") {
		t.Fatalf("redirect off the verified address error = %v", err)
	}
	if result, err := get(owner(apis), "/redirect-home"); err != nil || result["text"] != "server:/data" {
		t.Fatalf("redirect on the verified address = %#v, %v", result, err)
	}
	// A scoped socket also needs the address among its targets.
	if _, err := get(scopedServersAuth("192.168.1.200:9018"), "/data"); err == nil {
		t.Fatal("a scoped socket must not reach a verified server outside its targets")
	}
	if result, err := get(scopedServersAuth(address), "/data"); err != nil || result["text"] != "server:/data" {
		t.Fatalf("scoped request to its verified target = %#v, %v", result, err)
	}
	// The rule expires 60 s after the last successful challenge.
	agent.servers.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	if _, err := get(owner(apis), "/data"); err == nil {
		t.Fatal("a stale verification must not let HTTP through")
	}
}

func TestManifestServerConnect(t *testing.T) {
	detail := map[string]any{"latestRevision": map[string]any{"manifest": map[string]any{
		"name": "Form & Fuel",
		"backendPermissions": []any{
			map[string]any{"permission": "servers.connect", "reason": "journal",
				"apis":  []any{map[string]any{"api": testServerAPI, "min": 1.0, "max": 1.0}, map[string]any{"api": "Bad Id"}},
				"probe": map[string]any{"ports": []any{9018.0, 0.0, 70000.0}}},
		},
	}}}
	resolved, err := appFromDynerManifest("amit-bet/formfuel", detail)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.ServerAPIs) != 1 || resolved.ServerAPIs[0] != (ServerAPI{API: testServerAPI, Min: 1, Max: 1}) {
		t.Fatalf("server apis = %#v", resolved.ServerAPIs)
	}
	if len(resolved.ServerProbePorts) != 1 || resolved.ServerProbePorts[0] != 9018 {
		t.Fatalf("probe ports = %#v", resolved.ServerProbePorts)
	}
	if !socketAllows(resolved.Declared, serversPermission) {
		t.Fatal("servers.connect must be declared")
	}
	auth := authForIdentity(Config{}, BrowserIdentity{KeyID: "k"}, &resolved)
	if len(auth.serverAPIs) != 1 || len(auth.serverProbePorts) != 1 {
		t.Fatalf("socket auth server apis = %#v %#v", auth.serverAPIs, auth.serverProbePorts)
	}
}

func TestMDNSBrowserParsesResponses(t *testing.T) {
	browser := &mdnsBrowser{instances: map[string]*mdnsInstance{}, addresses: map[string][]net.IP{}}
	packet := buildTestMDNSResponse(t)
	browser.absorb(packet)
	candidates := browser.candidates()
	if len(candidates) != 1 || candidates[0].address != "192.168.1.81:9018" || candidates[0].scheme != "http" || candidates[0].source != "mdns" || len(candidates[0].instanceID) != 43 {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func buildTestMDNSResponse(t *testing.T) []byte {
	t.Helper()
	name := func(value string) dnsmessage.Name { return dnsmessage.MustNewName(value) }
	instance := "Journal._dynapp._tcp.local."
	header := func(n string, kind dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: name(n), Type: kind, Class: dnsmessage.ClassINET, TTL: 120}
	}
	message := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{
			{Header: header(dynappMDNSService, dnsmessage.TypePTR), Body: &dnsmessage.PTRResource{PTR: name(instance)}},
		},
		Additionals: []dnsmessage.Resource{
			{Header: header(instance, dnsmessage.TypeSRV), Body: &dnsmessage.SRVResource{Target: name("nas.local."), Port: 9018}},
			{Header: header(instance, dnsmessage.TypeTXT), Body: &dnsmessage.TXTResource{TXT: []string{"v=1", "id=" + strings.Repeat("a", 43), "scheme=http"}}},
			{Header: header("nas.local.", dnsmessage.TypeA), Body: &dnsmessage.AResource{A: [4]byte{192, 168, 1, 81}}},
		},
	}
	packet, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}
