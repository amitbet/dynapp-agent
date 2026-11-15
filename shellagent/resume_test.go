package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

const testRelayTokenHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func resumeTestAgent(capabilities ...string) *Server {
	return &Server{Config: Config{BrowserIdentities: []BrowserIdentity{
		{KeyID: "browser-test", Origin: "https://app.example", Capabilities: capabilities},
		{KeyID: "browser-other", Origin: "https://app.example", Capabilities: capabilities},
	}}}
}

// testRelay stands in for the relay Durable Object: frames it sends are
// tagged with a browser session id, frames it receives are untagged.
type testRelay struct {
	conn   *websocket.Conn
	frames chan []byte
}

func startTestRelay(t *testing.T, agent *Server, e2ee bool) *testRelay {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		relay := RelayConnection{AcceptedAuthTokenHashes: []string{testRelayTokenHash}}
		relay.Transport.E2EEncryption = e2ee
		agent.handleRelaySocket(r.Context(), connection, relay, Config{EnvironmentID: "env_test"})
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	connection.SetReadLimit(64 << 20)
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "") })
	relay := &testRelay{conn: connection, frames: make(chan []byte, 4096)}
	go func() {
		for {
			_, data, err := connection.Read(ctx)
			if err != nil {
				return
			}
			relay.frames <- data
		}
	}()
	return relay
}

func (r *testRelay) send(t *testing.T, session string, value any) {
	t.Helper()
	payload, _ := json.Marshal(value)
	envelope, _ := json.Marshal(map[string]any{"type": "dynapp-relay-frame", "session": session, "payload": string(payload)})
	if err := r.conn.Write(context.Background(), websocket.MessageText, envelope); err != nil {
		t.Fatal(err)
	}
}

// next returns the next frame that is not a session-ack.
func (r *testRelay) next(t *testing.T, what string) map[string]any {
	t.Helper()
	return nextSessionFrame(t, r.frames, what)
}

func nextSessionFrame(t *testing.T, frames <-chan []byte, what string) map[string]any {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case data := <-frames:
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("%s is not JSON: %v", what, err)
			}
			if decoded["type"] == "session-ack" {
				continue
			}
			return decoded
		case <-deadline:
			t.Fatalf("no %s arrived", what)
			return nil
		}
	}
}

func relayHello(keyID string, session map[string]any) map[string]any {
	hello := map[string]any{"type": "hello", "protocol": ProtocolVersion, "tokenHash": testRelayTokenHash, "keyId": keyID}
	if session != nil {
		hello["session"] = session
	}
	return hello
}

func sessionCredentials(t *testing.T, reply map[string]any) (string, string) {
	t.Helper()
	session, _ := reply["session"].(map[string]any)
	id, _ := session["id"].(string)
	token, _ := session["token"].(string)
	if reply["ok"] != true || len(id) < 22 || len(token) != 43 || session["graceMs"] == nil {
		t.Fatalf("hello reply carries no resumable session: %#v", reply)
	}
	features, _ := reply["features"].(map[string]any)
	if features["resume"] != true || features["reliableStreams"] != false || features["rawBridgeStreams"] != false || features["filesystemStreams"] != false {
		t.Fatalf("resumable session features = %#v", features)
	}
	return id, token
}

// A streaming process keeps running while the browser's WebRTC peer dies,
// and a new peer resumes the session without losing or repeating a frame.
func TestResumableWebRTCSessionSurvivesPeerLoss(t *testing.T) {
	agent := resumeTestAgent("fs.execFile")
	first := dialICEBrowser(t, agent)
	if err := first.control.SendText(`{"type":"hello","protocol":2,"keyId":"browser-test","fragments":true,"session":{"resumable":true}}`); err != nil {
		t.Fatal(err)
	}
	id, token := sessionCredentials(t, nextSessionFrame(t, first.messages, "hello reply"))
	const lines = 150
	script := fmt.Sprintf("i=1; while [ $i -le %d ]; do echo line$i; i=$((i+1)); sleep 0.01; done", lines)
	request, _ := json.Marshal(map[string]any{"type": "exec", "id": "stream", "stream": true, "file": "/bin/sh", "args": []string{"-c", script}})
	if err := first.control.SendText(string(request)); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	starts, exits := 0, 0
	received := 0
	collect := func(frame map[string]any) {
		received++
		switch frame["type"] {
		case "exec-start":
			starts++
		case "exec-output":
			if frame["stream"] == "stdout" {
				stdout.WriteString(frame["data"].(string))
			}
		case "exec-exit":
			exits++
		}
	}
	for !strings.Contains(stdout.String(), "line20\n") {
		collect(nextSessionFrame(t, first.messages, "process output"))
	}
	// Frames still queued on the dead peer were never processed, so the
	// resume reports only what was.
	_ = first.peer.Close()

	second := dialICEBrowser(t, agent)
	resume, _ := json.Marshal(map[string]any{"type": "hello", "protocol": 2, "keyId": "browser-test", "fragments": true, "session": map[string]any{"resume": id, "token": token, "received": received}})
	if err := second.control.SendText(string(resume)); err != nil {
		t.Fatal(err)
	}
	reply := nextSessionFrame(t, second.messages, "resume reply")
	session, _ := reply["session"].(map[string]any)
	if reply["resumed"] != true || reply["ok"] != true || session["id"] != id || session["received"] != float64(1) {
		t.Fatalf("resume reply = %#v", reply)
	}
	for exits == 0 {
		collect(nextSessionFrame(t, second.messages, "replayed output"))
	}
	var want strings.Builder
	for index := 1; index <= lines; index++ {
		fmt.Fprintf(&want, "line%d\n", index)
	}
	if stdout.String() != want.String() || starts != 1 || exits != 1 {
		t.Fatalf("stdout after resume (starts=%d exits=%d):\n%q", starts, exits, stdout.String())
	}
}

// A WebRTC carrier that opens while the session runs on the relay takes it
// over, and the relay carrier is told the session moved.
func TestRelaySessionUpgradesToWebRTC(t *testing.T) {
	agent := resumeTestAgent("fs.home")
	relay := startTestRelay(t, agent, false)
	relay.send(t, "browser-a", relayHello("browser-test", map[string]any{"resumable": true}))
	reply := relay.next(t, "relay hello reply")
	id, token := sessionCredentials(t, reply)
	if environment, _ := reply["environment"].(map[string]any); environment["source"] != "relay" {
		t.Fatalf("relay environment = %#v", reply["environment"])
	}
	relay.send(t, "browser-a", map[string]any{"type": "ping", "id": "over-relay"})
	if pong := relay.next(t, "relay pong"); pong["type"] != "pong" || pong["id"] != "over-relay" {
		t.Fatalf("relay pong = %#v", pong)
	}

	browser := dialICEBrowser(t, agent)
	resume, _ := json.Marshal(map[string]any{"type": "hello", "protocol": 2, "keyId": "browser-test", "session": map[string]any{"resume": id, "token": token, "received": 1}})
	if err := browser.control.SendText(string(resume)); err != nil {
		t.Fatal(err)
	}
	resumed := nextSessionFrame(t, browser.messages, "resume reply")
	if resumed["resumed"] != true || resumed["session"].(map[string]any)["received"] != float64(1) {
		t.Fatalf("resume reply = %#v", resumed)
	}
	if features, _ := resumed["features"].(map[string]any); features["resume"] != true || features["reliableStreams"] != false {
		t.Fatalf("resumed features = %#v", resumed["features"])
	}
	if moved := relay.next(t, "relay close"); moved["type"] != "error" || moved["code"] != float64(closeSessionMoved) {
		t.Fatalf("relay carrier was not told the session moved: %#v", moved)
	}
	if err := browser.control.SendText(`{"type":"ping","id":"over-webrtc"}`); err != nil {
		t.Fatal(err)
	}
	if pong := nextSessionFrame(t, browser.messages, "WebRTC pong"); pong["type"] != "pong" || pong["id"] != "over-webrtc" {
		t.Fatalf("WebRTC pong = %#v", pong)
	}
	// The relay session id is retired: its frames reach nothing.
	relay.send(t, "browser-a", map[string]any{"type": "ping", "id": "stale"})
	select {
	case data := <-relay.frames:
		t.Fatalf("retired relay session answered: %s", data)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestResumeRejectsBadTokensIdentityAndExpiredSessions(t *testing.T) {
	agent := resumeTestAgent("fs.home")
	agent.resumeGraceOverride = 300 * time.Millisecond
	relay := startTestRelay(t, agent, false)
	relay.send(t, "browser-a", relayHello("browser-test", map[string]any{"resumable": true}))
	id, token := sessionCredentials(t, relay.next(t, "hello reply"))

	expectExpired := func(session string, hello map[string]any) {
		t.Helper()
		relay.send(t, session, hello)
		if frame := relay.next(t, "resume rejection"); frame["type"] != "error" || frame["code"] != float64(closeSessionExpired) {
			t.Fatalf("%s: resume rejection = %#v", session, frame)
		}
	}
	// browser-b replaces browser-a's relay socket, so the session detaches.
	expectExpired("browser-b", relayHello("browser-test", map[string]any{"resume": id, "token": strings.Repeat("A", 43), "received": 0}))
	expectExpired("browser-c", relayHello("browser-other", map[string]any{"resume": id, "token": token, "received": 0}))
	expectExpired("browser-d", relayHello("browser-test", map[string]any{"resume": "unknown-session-id-0000", "token": token, "received": 0}))
	expectExpired("browser-e", relayHello("browser-test", map[string]any{"resume": id, "token": token, "received": 5}))
	time.Sleep(600 * time.Millisecond)
	expectExpired("browser-f", relayHello("browser-test", map[string]any{"resume": id, "token": token, "received": 0}))
	if agent.lookupSessionSocket(id) != nil {
		t.Fatal("expired session is still registered")
	}
}

// Each relay browser session id is its own session: a new browser socket
// gets fresh state while the replaced one's session waits to be resumed.
func TestRelayBrowserSessionsAreIndependent(t *testing.T) {
	agent := resumeTestAgent("fs.execFile")
	relay := startTestRelay(t, agent, false)
	relay.send(t, "browser-a", relayHello("browser-test", map[string]any{"resumable": true}))
	id, token := sessionCredentials(t, relay.next(t, "first hello reply"))
	relay.send(t, "browser-a", map[string]any{"type": "exec", "id": "reader", "stream": true, "file": "/bin/sh", "args": []string{"-c", "read value; echo got:$value"}})
	started := relay.next(t, "exec start")
	processID, _ := started["processId"].(string)
	if started["type"] != "exec-start" || processID == "" {
		t.Fatalf("exec start = %#v", started)
	}

	relay.send(t, "browser-b", relayHello("browser-test", map[string]any{"resumable": true}))
	otherID, _ := sessionCredentials(t, relay.next(t, "second hello reply"))
	if otherID == id {
		t.Fatal("a second browser session reused the first session id")
	}
	relay.send(t, "browser-b", map[string]any{"type": "process", "id": "foreign", "processId": processID, "action": "write", "data": "intruder\n"})
	relay.send(t, "browser-b", map[string]any{"type": "ping", "id": "b"})
	for {
		frame := relay.next(t, "second session pong")
		if strings.Contains(fmt.Sprint(frame["data"]), "intruder") {
			t.Fatalf("second session reached the first session's process: %#v", frame)
		}
		if frame["type"] == "pong" && frame["id"] == "b" {
			break
		}
	}

	relay.send(t, "browser-c", relayHello("browser-test", map[string]any{"resume": id, "token": token, "received": 1}))
	if resumed := relay.next(t, "resume reply"); resumed["resumed"] != true || resumed["session"].(map[string]any)["received"] != float64(1) {
		t.Fatalf("resume reply = %#v", resumed)
	}
	relay.send(t, "browser-c", map[string]any{"type": "process", "id": "write", "processId": processID, "action": "write", "data": "hello\n"})
	var output strings.Builder
	for {
		frame := relay.next(t, "resumed process output")
		if frame["type"] == "exec-output" {
			output.WriteString(fmt.Sprint(frame["data"]))
		}
		if frame["type"] == "exec-exit" {
			break
		}
	}
	if output.String() != "got:hello\n" {
		t.Fatalf("resumed process output = %q", output.String())
	}
}

// Hellos without a session, or without a browser identity, keep today's
// non-resumable sessions and carrier features.
func TestNonResumableHellosBehaveAsBefore(t *testing.T) {
	agent := resumeTestAgent("fs.home")
	_, messages, _ := connectedICEBrowser(t, agent, `{"type":"hello","protocol":2,"keyId":"browser-test"}`)
	reply := receiveICEMessage(t, messages, "hello reply")
	features, _ := reply["features"].(map[string]any)
	if reply["ok"] != true || reply["session"] != nil || features["resume"] != nil || features["reliableStreams"] != true || features["filesystemStreams"] != true {
		t.Fatalf("plain WebRTC hello reply = %#v", reply)
	}

	relay := startTestRelay(t, agent, false)
	relay.send(t, "browser-a", relayHello("", map[string]any{"resumable": true}))
	reply = relay.next(t, "relay hello reply")
	if reply["ok"] != true || reply["session"] != nil {
		t.Fatalf("a relay hello without an identity must not be resumable: %#v", reply)
	}
	relay.send(t, "browser-a", map[string]any{"type": "ping", "id": "plain"})
	if pong := relay.next(t, "pong"); pong["type"] != "pong" {
		t.Fatalf("pong = %#v", pong)
	}
	// A replaced non-resumable relay session ends; the new one starts fresh.
	relay.send(t, "browser-b", map[string]any{"type": "ping", "id": "unauthenticated"})
	if rejected := relay.next(t, "rejection"); rejected["error"] != "Pairing credential rejected" {
		t.Fatalf("rejection = %#v", rejected)
	}
}

// Legacy end-to-end encryption still works inside a relay virtual socket and
// never yields a resumable session.
func TestRelayE2EESessionsStayNonResumable(t *testing.T) {
	agent := resumeTestAgent("fs.home")
	relay := startTestRelay(t, agent, true)
	relay.send(t, "browser-a", map[string]any{"type": "dynapp-e2ee-init", "version": 1, "keyId": remoteKeyID(testRelayTokenHash)})
	if ready := relay.next(t, "e2ee ready"); ready["type"] != "dynapp-e2ee-ready" {
		t.Fatalf("e2ee ready = %#v", ready)
	}
	sealed := func(value any) string {
		payload, _ := json.Marshal(value)
		envelope, err := sealRemoteFrame(testRelayTokenHash, payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(envelope)
	}
	sendSealed := func(value any) {
		envelope, _ := json.Marshal(map[string]any{"type": "dynapp-relay-frame", "session": "browser-a", "payload": sealed(value)})
		if err := relay.conn.Write(context.Background(), websocket.MessageText, envelope); err != nil {
			t.Fatal(err)
		}
	}
	openFrame := func(what string) map[string]any {
		frame := relay.next(t, what)
		wire, _ := json.Marshal(frame)
		plain, err := openRemoteFrame(testRelayTokenHash, wire)
		if err != nil {
			t.Fatalf("%s is not sealed: %#v", what, frame)
		}
		var decoded map[string]any
		_ = json.Unmarshal(plain, &decoded)
		return decoded
	}
	sendSealed(relayHello("browser-test", map[string]any{"resumable": true}))
	if reply := openFrame("hello reply"); reply["ok"] != true || reply["session"] != nil {
		t.Fatalf("E2EE hello reply = %#v", reply)
	}
	sendSealed(map[string]any{"type": "ping", "id": "sealed"})
	if pong := openFrame("pong"); pong["type"] != "pong" || pong["id"] != "sealed" {
		t.Fatalf("E2EE pong = %#v", pong)
	}
}

func TestICEServersComeFromDynerWithFallback(t *testing.T) {
	var requests atomic.Int32
	var expiresAt atomic.Value
	expiresAt.Store(time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339))
	failing := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/remote-environments/env_test/ice/servers" || r.Header.Get("Authorization") != "DynApp-Device device-secret" {
			t.Errorf("unexpected ICE servers request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iceServers": []any{
				map[string]any{"urls": []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}},
				map[string]any{"urls": "turn:turn.cloudflare.com:3478?transport=udp", "username": "user", "credential": "secret"},
				map[string]any{"urls": []string{"turn:no-credentials.example:3478", "not a url"}},
			},
			"expiresAt": expiresAt.Load(),
		})
	}))
	defer server.Close()
	config := Config{DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret"}
	agent := &Server{}

	servers := agent.iceServers(context.Background(), config)
	if len(servers) != 2 || len(servers[0].URLs) != 2 || servers[1].Username != "user" || servers[1].Credential != "secret" {
		t.Fatalf("ICE servers = %#v", servers)
	}
	agent.iceServers(context.Background(), config)
	if requests.Load() != 1 {
		t.Fatalf("a cached list was fetched %d times", requests.Load())
	}

	// Inside the refresh margin the list is fetched again.
	agent.iceServersMu.Lock()
	agent.iceServersCache = iceServerCache{}
	agent.iceServersMu.Unlock()
	expiresAt.Store(time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339))
	agent.iceServers(context.Background(), config)
	agent.iceServers(context.Background(), config)
	if requests.Load() != 3 {
		t.Fatalf("a list about to expire was cached: %d requests", requests.Load())
	}

	failing.Store(true)
	agent.iceServersMu.Lock()
	agent.iceServersCache = iceServerCache{}
	agent.iceServersMu.Unlock()
	fallback := agent.iceServers(context.Background(), config)
	if len(fallback) != 1 || strings.Join(fallback[0].URLs, ",") != "stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302" {
		t.Fatalf("fallback ICE servers = %#v", fallback)
	}
	if unenrolled := (&Server{}).iceServers(context.Background(), Config{}); len(unenrolled) != 1 || len(unenrolled[0].URLs) != 2 {
		t.Fatalf("unenrolled ICE servers = %#v", unenrolled)
	}
}

// memoryCarrier is an in-process carrier for session-core unit tests.
type memoryCarrier struct {
	incoming chan dataChannelMessage
	mu       sync.Mutex
	written  [][]byte
	closed   chan struct{}
	once     sync.Once
}

func newMemoryCarrier() *memoryCarrier {
	return &memoryCarrier{incoming: make(chan dataChannelMessage, 16), closed: make(chan struct{})}
}

func (c *memoryCarrier) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case message := <-c.incoming:
		return message.kind, message.data, nil
	case <-c.closed:
		return 0, nil, errors.New("closed")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *memoryCarrier) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	select {
	case <-c.closed:
		return errors.New("closed")
	default:
	}
	c.mu.Lock()
	c.written = append(c.written, append([]byte(nil), data...))
	c.mu.Unlock()
	return nil
}

func (c *memoryCarrier) Close(websocket.StatusCode, string) error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func TestSessionReplayBufferAcknowledgementAndOverflow(t *testing.T) {
	agent := &Server{}
	session, err := agent.newSessionSocket(context.Background(), socketAuthentication{keyID: "browser-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.end(websocket.StatusNormalClosure, "")
	carrier := newMemoryCarrier()
	if err := session.attach(carrier, context.Background(), carrierCapabilities{}, 0, func(uint64) map[string]any { return map[string]any{"type": "hello"} }); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if err := session.Write(context.Background(), websocket.MessageText, []byte(`{"type":"pong"}`)); err != nil {
			t.Fatal(err)
		}
	}
	carrier.incoming <- dataChannelMessage{kind: websocket.MessageText, data: []byte(`{"type":"session-ack","received":2}`)}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		buffered := len(session.replay)
		session.mu.Unlock()
		if buffered == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("acknowledged frames stayed buffered: %d", buffered)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Browser frames are counted and acknowledged within a second.
	carrier.incoming <- dataChannelMessage{kind: websocket.MessageText, data: []byte(`{"type":"ping","id":"1"}`)}
	if _, data, err := session.Read(context.Background()); err != nil || !bytes.Contains(data, []byte(`"ping"`)) {
		t.Fatalf("session read = %s, %v", data, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		carrier.mu.Lock()
		last := carrier.written[len(carrier.written)-1]
		carrier.mu.Unlock()
		if string(last) == `{"received":1,"type":"session-ack"}` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session-ack for the browser frame; last write %s", last)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Past the replay limit the session keeps working but cannot resume.
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for index := 0; index < 17; index++ {
		if err := session.Write(context.Background(), websocket.MessageBinary, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.attach(newMemoryCarrier(), context.Background(), carrierCapabilities{}, 3, func(uint64) map[string]any { return nil }); err == nil {
		t.Fatal("an overflowed session was resumed")
	}
	_ = carrier.Close(websocket.StatusNormalClosure, "")
	select {
	case <-session.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an overflowed session outlived its carrier")
	}
}

// A UDP bridge bound to datagrams keeps flowing on the control channel when
// the session's current carrier has no datagram channel.
func TestDatagramBridgeFallsBackToControlChannel(t *testing.T) {
	agent := &Server{}
	session, err := agent.newSessionSocket(context.Background(), socketAuthentication{keyID: "browser-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.end(websocket.StatusNormalClosure, "")
	carrier := newMemoryCarrier()
	if err := session.attach(carrier, context.Background(), carrierCapabilities{}, 0, func(uint64) map[string]any { return map[string]any{"type": "hello"} }); err != nil {
		t.Fatal(err)
	}
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	bridge, err := dialUDPBridge("127.0.0.1", target.LocalAddr().(*net.UDPAddr).Port, 0)
	if err != nil {
		t.Fatal(err)
	}
	bridges := newBridgeSet()
	defer bridges.closeAll()
	bridges.put("udp_1", bridge, session)
	if !bridges.bindDatagrams("udp_1") {
		t.Fatal("bridge did not bind datagrams")
	}
	go agent.forwardTCPBridge(session, bridges, "udp_1", bridge)
	if _, err := target.WriteToUDP([]byte("packet"), bridge.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		carrier.mu.Lock()
		var frame []byte
		for _, written := range carrier.written {
			if bytes.HasPrefix(written, []byte("DFB1")) {
				frame = written
			}
		}
		carrier.mu.Unlock()
		if frame != nil {
			header, body, err := decodeBinaryFrame(frame)
			if err != nil || header.Type != "net-data" || header.BridgeID != "udp_1" || string(body) != "packet" {
				t.Fatalf("fallback frame = %#v %q %v", header, body, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("datagram did not fall back to a net-data frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
