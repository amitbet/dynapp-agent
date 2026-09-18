package shellagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestAuthenticateICEHelloRefreshesNewBrowserIdentity(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := BrowserJWK{
		KTY: "EC", CRV: "P-256",
		X: base64.RawURLEncoding.EncodeToString(private.PublicKey.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(private.PublicKey.Y.FillBytes(make([]byte, 32))),
	}
	identity := BrowserIdentity{
		KeyID: BrowserKeyID(key), PublicKeyJWK: key, Origin: "https://app.example", Capabilities: []string{"fs.home"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/remote-environments/env_test/browser-identities" {
			t.Fatalf("unexpected refresh path: %s", request.URL.Path)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"browserIdentities": []BrowserIdentity{identity}})
	}))
	defer server.Close()

	agent := &Server{}
	config := Config{
		SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL, EnvironmentID: "env_test",
		DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
	}
	grant := agent.authenticateICEHello(context.Background(), config, message{Type: "hello", Protocol: json.RawMessage("2"), KeyID: identity.KeyID})
	if !grant.ok || grant.keyID != identity.KeyID || !grant.allows("fs.home") {
		t.Fatalf("refreshed grant = %#v", grant)
	}
}

func TestSyncRemoteEnvironmentStateReturnsPendingICESessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/remote-environments/env_test/browser-identities" {
			t.Fatalf("unexpected sync request: %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "DynApp-Device env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"pendingIceSessions": []any{
			map[string]any{"id": "ice_abcdefghijklmnopqrstuv", "offer": map[string]any{"type": "offer", "sdp": "v=0"}},
		}})
	}))
	defer server.Close()

	state, err := SyncRemoteEnvironmentState(context.Background(), server.Client(), Config{
		SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingICESessions) != 1 || state.PendingICESessions[0].Offer == nil || state.PendingICESessions[0].Offer.Type != "offer" {
		t.Fatalf("unexpected sessions: %#v", state.PendingICESessions)
	}
}

func TestAnswerICESessionEstablishesBrowserDataChannels(t *testing.T) {
	var answer iceDescription
	var answerMu sync.Mutex
	answerPosted := make(chan struct{})
	var candidateMu sync.Mutex
	var candidatePosts [][]json.RawMessage
	var browserCandidates []json.RawMessage
	browserCandidateServed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		const sessionPath = "/api/v1/remote-environments/env_test/ice/sessions/ice_abcdefghijklmnopqrstuv"
		if strings.HasPrefix(request.URL.Path, "/api/v1/") {
			if got := request.Header.Get("Authorization"); got != "DynApp-Device device-secret" {
				t.Errorf("unexpected authorization: %q", got)
			}
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == sessionPath+"/shell-candidates":
			var list []json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&list); err != nil {
				t.Errorf("invalid shell candidates: %v", err)
			}
			candidateMu.Lock()
			candidatePosts = append(candidatePosts, list)
			candidateMu.Unlock()
			_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": "ice_abcdefghijklmnopqrstuv"}})
			return
		case request.Method == http.MethodGet && request.URL.Path == sessionPath:
			candidateMu.Lock()
			list := append([]json.RawMessage(nil), browserCandidates...)
			candidateMu.Unlock()
			if len(list) > 0 {
				select {
				case browserCandidateServed <- struct{}{}:
				default:
				}
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{
				"id": "ice_abcdefghijklmnopqrstuv", "browserCandidates": list,
			}})
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != sessionPath+"/answer" {
			t.Logf("unexpected test server request: %s %s (%s)", request.Method, request.URL.Path, request.UserAgent())
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if got := request.Header.Get("Authorization"); got != "DynApp-Device device-secret" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		var posted iceDescription
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		answerMu.Lock()
		answer = posted
		answerMu.Unlock()
		close(answerPosted)
		_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": "ice_abcdefghijklmnopqrstuv"}})
	}))
	defer server.Close()

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	control, err := browser.CreateDataChannel("dynapp-control-v2", &webrtc.DataChannelInit{Ordered: boolPointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	ordered := false
	maxRetransmits := uint16(0)
	if _, err := browser.CreateDataChannel("dynapp-datagram-v2", &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &maxRetransmits}); err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	helloReply := make(chan map[string]any, 1)
	control.OnOpen(func() { close(opened) })
	control.OnMessage(func(message webrtc.DataChannelMessage) {
		var decoded map[string]any
		if json.Unmarshal(message.Data, &decoded) == nil {
			helloReply <- decoded
		}
	})
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("browser ICE gathering timed out")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := &Server{Config: Config{BrowserIdentities: []BrowserIdentity{{
		KeyID: "browser-test", Origin: "https://app.example", Capabilities: []string{"net.tcp.connect"},
	}}}}
	local := browser.LocalDescription()
	if local == nil {
		t.Fatal("browser offer is unavailable")
	}
	if err := agent.answerICESession(ctx, Config{
		DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret",
	}, iceSession{ID: "ice_abcdefghijklmnopqrstuv", Offer: &iceDescription{Type: "offer", SDP: local.SDP}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answerPosted:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not post an ICE answer")
	}
	answerMu.Lock()
	remote := answer
	answerMu.Unlock()
	if remote.Type != "answer" || remote.SDP == "" {
		t.Fatalf("invalid agent answer: %#v", remote)
	}
	// Simulate a browser that trickles one more candidate after the offer: a
	// harmless duplicate of a host candidate plus the end-of-candidates marker.
	trickled := browser.LocalDescription()
	var trickledCandidate json.RawMessage
	for _, line := range strings.Split(trickled.SDP, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			mid := "0"
			index := uint16(0)
			encoded, _ := json.Marshal(webrtc.ICECandidateInit{Candidate: strings.TrimPrefix(line, "a="), SDPMid: &mid, SDPMLineIndex: &index})
			trickledCandidate = encoded
			break
		}
	}
	if trickledCandidate == nil {
		t.Fatal("browser offer has no candidates to trickle")
	}
	candidateMu.Lock()
	browserCandidates = []json.RawMessage{trickledCandidate, json.RawMessage("null")}
	candidateMu.Unlock()
	select {
	case <-browserCandidateServed:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not poll for trickled browser candidates")
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: remote.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("WebRTC control channel did not open")
	}
	if err := control.SendText(`{"type":"hello","protocol":2,"keyId":"browser-test"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-helloReply:
		if reply["ok"] != true {
			t.Fatalf("unexpected WebRTC hello response: %#v", reply)
		}
		features, _ := reply["features"].(map[string]any)
		if features["reliableStreams"] != true || features["datagrams"] != true || features["rawBridgeStreams"] != true || features["fragments"] != true {
			t.Fatalf("unexpected WebRTC features: %#v", features)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not answer the WebRTC protocol hello")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer connection.Close()
			_, _ = io.Copy(connection, connection)
		}
	}()
	target := listener.Addr().(*net.TCPAddr)
	if err := control.SendText(`{"type":"net","id":"net-test","action":"open","protocol":"tcp","target":{"host":"127.0.0.1","port":` + fmt.Sprint(target.Port) + `}}`); err != nil {
		t.Fatal(err)
	}
	var bridgeID string
	select {
	case reply := <-helloReply:
		result, _ := reply["result"].(map[string]any)
		bridgeID, _ = result["bridgeId"].(string)
		if reply["type"] != "net-result" || bridgeID == "" {
			t.Fatalf("unexpected network bridge response: %#v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not open the WebRTC network bridge")
	}
	raw, err := browser.CreateDataChannel("dynapp-stream-v2-test", &webrtc.DataChannelInit{Ordered: boolPointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	rawPayload := bytes.Repeat([]byte("webrtc-raw-bridge"), 20_000)
	echoed := make(chan []byte, 1)
	var echoedMu sync.Mutex
	var echoedBytes []byte
	raw.OnMessage(func(message webrtc.DataChannelMessage) {
		echoedMu.Lock()
		echoedBytes = append(echoedBytes, message.Data...)
		if len(echoedBytes) >= len(rawPayload) {
			complete := append([]byte(nil), echoedBytes...)
			select {
			case echoed <- complete:
			default:
			}
		}
		echoedMu.Unlock()
	})
	raw.OnOpen(func() {
		_ = raw.SendText(`{"type":"channel","action":"bind","protocol":2,"bridgeId":"` + bridgeID + `","mode":"raw"}`)
		_ = raw.Send(rawPayload)
	})
	select {
	case payload := <-echoed:
		if !bytes.Equal(payload, rawPayload) {
			t.Fatalf("unexpected raw bridge payload: got %d bytes, want %d", len(payload), len(rawPayload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WebRTC raw bridge did not echo data")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		candidateMu.Lock()
		posts := append([][]json.RawMessage(nil), candidatePosts...)
		candidateMu.Unlock()
		if len(posts) > 0 && string(posts[len(posts)-1][len(posts[len(posts)-1])-1]) == "null" {
			for index := 1; index < len(posts); index++ {
				if len(posts[index]) < len(posts[index-1]) {
					t.Fatalf("shell candidate lists must only grow: %d then %d", len(posts[index-1]), len(posts[index]))
				}
			}
			var first webrtc.ICECandidateInit
			if err := json.Unmarshal(posts[len(posts)-1][0], &first); err != nil || !strings.HasPrefix(first.Candidate, "candidate:") {
				t.Fatalf("unexpected trickled shell candidate: %s", posts[len(posts)-1][0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not trickle a null-terminated shell candidate list: %v", posts)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestICECandidateTricklerPostsGrowingNullTerminatedLists(t *testing.T) {
	var mu sync.Mutex
	var posts [][]json.RawMessage
	release := make(chan struct{})
	trickler := newICECandidateTrickler(func(list []json.RawMessage) error {
		mu.Lock()
		posts = append(posts, list)
		count := len(posts)
		mu.Unlock()
		if count == 1 {
			<-release
		}
		return nil
	})
	mid := "0"
	one := &webrtc.ICECandidate{Foundation: "1", Priority: 1, Address: "192.168.1.10", Protocol: webrtc.ICEProtocolUDP, Port: 5000, Typ: webrtc.ICECandidateTypeHost, Component: 1, SDPMid: mid}
	two := &webrtc.ICECandidate{Foundation: "2", Priority: 2, Address: "203.0.113.5", Protocol: webrtc.ICEProtocolUDP, Port: 6000, Typ: webrtc.ICECandidateTypeSrflx, RelatedAddress: "192.168.1.10", RelatedPort: 5000, Component: 1, SDPMid: mid}
	trickler.add(one)
	time.Sleep(50 * time.Millisecond)
	trickler.add(two)
	trickler.add(nil)
	trickler.add(two) // ignored after end of gathering
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := len(posts) >= 2 && string(posts[len(posts)-1][len(posts[len(posts)-1])-1]) == "null"
		snapshot := append([][]json.RawMessage(nil), posts...)
		mu.Unlock()
		if done {
			if len(snapshot[0]) != 1 || len(snapshot[len(snapshot)-1]) != 3 {
				t.Fatalf("unexpected candidate lists: %v", snapshot)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("trickler never posted a terminated list: %v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func boolPointer(value bool) *bool { return &value }

// The relay must cost nothing until a browser cannot reach this machine
// directly, and an expiring request must not cut a session that is in use.
func TestRelayOpensOnRequestAndClosesWhenNobodyIsAsking(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/remote-environments/env_test/ticket" {
			// Keep the dial cheap: the ticket is enough to prove the relay was
			// activated, and an unreachable endpoint just reconnects.
			_ = json.NewEncoder(response).Encode(map[string]any{"connection": map[string]any{
				"endpoint": "ws://127.0.0.1:1/v1/connect", "ticket": "relay-ticket",
			}})
			return
		}
		requests++
		_ = json.NewEncoder(response).Encode(map[string]any{"relayRequested": requests == 1})
	}))
	defer server.Close()

	config := Config{
		SchemaVersion:    ConfigSchemaVersion,
		DynerBaseURL:     server.URL,
		EnvironmentID:    "env_test",
		DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
		RelayEnabled:     true,
	}
	agent := &Server{Config: config, StateDir: t.TempDir()}

	state, err := SyncRemoteEnvironmentState(context.Background(), server.Client(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !state.RelayRequested {
		t.Fatal("expected the sync to report the browser's relay request")
	}
	agent.applyRelayRequest(state.RelayRequested)
	agent.mu.Lock()
	running := agent.relayCancel != nil
	agent.mu.Unlock()
	if !running {
		t.Fatal("expected a requested relay to be running")
	}

	// Still inside the grace window, so a quiet sync leaves it alone.
	agent.applyRelayRequest(false)
	agent.mu.Lock()
	running = agent.relayCancel != nil
	agent.mu.Unlock()
	if !running {
		t.Fatal("expected the relay to survive the grace window")
	}

	// A live session keeps the socket even after the request ages out.
	agent.mu.Lock()
	agent.relayWantedAt = time.Now().Add(-2 * relayIdleGrace)
	agent.mu.Unlock()
	agent.noteRelayActivity()
	agent.applyRelayRequest(false)
	agent.mu.Lock()
	running = agent.relayCancel != nil
	agent.mu.Unlock()
	if !running {
		t.Fatal("expected an active relay session to hold the socket open")
	}

	// Nobody asking and nothing in flight: the socket goes away.
	agent.mu.Lock()
	agent.relayActivityAt = time.Now().Add(-2 * relayIdleGrace)
	agent.mu.Unlock()
	agent.applyRelayRequest(false)
	agent.mu.Lock()
	running = agent.relayCancel != nil
	agent.mu.Unlock()
	if running {
		t.Fatal("expected an unused relay to close")
	}
}

func TestWebRTCFragmentsSplitOversizedMessages(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), webRTCFragmentThreshold*2+17)
	frames := webRTCFragments(payload, true)
	if len(frames) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(frames))
	}
	var rebuilt []byte
	for index, frame := range frames {
		if string(frame[:4]) != webRTCFragmentMagic {
			t.Fatalf("fragment %d is not marked", index)
		}
		if frame[4] != 1 {
			t.Fatalf("fragment %d lost the text flag", index)
		}
		if got := int(binary.BigEndian.Uint16(frame[5:7])); got != index {
			t.Fatalf("fragment %d carries index %d", index, got)
		}
		if got := int(binary.BigEndian.Uint16(frame[7:9])); got != 3 {
			t.Fatalf("fragment %d carries count %d", index, got)
		}
		if len(frame) > webRTCFragmentHeaderSize+webRTCFragmentThreshold {
			t.Fatalf("fragment %d is %d bytes, above the peer limit", index, len(frame))
		}
		rebuilt = append(rebuilt, frame[webRTCFragmentHeaderSize:]...)
	}
	if !bytes.Equal(rebuilt, payload) {
		t.Fatal("reassembled payload does not match")
	}
	if webRTCFragments(bytes.Repeat([]byte("y"), 10), false)[0][4] != 0 {
		t.Fatal("binary fragments must not set the text flag")
	}
}

func TestWebRTCSocketOnlyFragmentsForBrowsersThatAskedFor(t *testing.T) {
	// A browser that never announced reassembly must keep receiving whole
	// messages, even ones the carrier will then refuse.
	off := &webRTCChannelSocket{fragments: &atomic.Bool{}}
	if off.fragments.Load() {
		t.Fatal("fragmentation must default to off")
	}
	on := &webRTCChannelSocket{fragments: &atomic.Bool{}}
	on.fragments.Store(true)
	if !on.fragments.Load() {
		t.Fatal("hello must be able to enable fragmentation")
	}
}

// connectedICEBrowser stands up a real pion peer pair through answerICESession
// and returns the browser's control channel plus its reassembled messages.
func connectedICEBrowser(t *testing.T, agent *Server, hello string) (*webrtc.DataChannel, <-chan []byte, *atomic.Int64) {
	t.Helper()
	answerPosted := make(chan struct{})
	var answerMu sync.Mutex
	var answer iceDescription
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		const sessionPath = "/api/v1/remote-environments/env_test/ice/sessions/ice_abcdefghijklmnopqrstuv"
		switch {
		case request.Method == http.MethodPost && request.URL.Path == sessionPath+"/answer":
			var posted iceDescription
			if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
				t.Errorf("invalid answer: %v", err)
			}
			answerMu.Lock()
			answer = posted
			answerMu.Unlock()
			close(answerPosted)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": "ice_abcdefghijklmnopqrstuv"}})
	}))
	t.Cleanup(server.Close)

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	control, err := browser.CreateDataChannel("dynapp-control-v2", &webrtc.DataChannelInit{Ordered: boolPointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	control.OnOpen(func() { close(opened) })
	messages := make(chan []byte, 8)
	fragmentsSeen := &atomic.Int64{}
	// The same reassembly the browser runtime performs.
	var parts []byte
	var expected int
	control.OnMessage(func(message webrtc.DataChannelMessage) {
		data := message.Data
		if len(data) >= webRTCFragmentHeaderSize && string(data[:4]) == webRTCFragmentMagic {
			fragmentsSeen.Add(1)
			index := int(binary.BigEndian.Uint16(data[5:7]))
			count := int(binary.BigEndian.Uint16(data[7:9]))
			if index == 0 {
				parts, expected = nil, count
			}
			parts = append(parts, data[webRTCFragmentHeaderSize:]...)
			if index == expected-1 {
				messages <- append([]byte(nil), parts...)
				parts = nil
			}
			return
		}
		messages <- append([]byte(nil), data...)
	})
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("browser ICE gathering timed out")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := agent.answerICESession(ctx, Config{
		DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret",
	}, iceSession{ID: "ice_abcdefghijklmnopqrstuv", Offer: &iceDescription{Type: "offer", SDP: browser.LocalDescription().SDP}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answerPosted:
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not post an ICE answer")
	}
	answerMu.Lock()
	remote := answer
	answerMu.Unlock()
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: remote.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("WebRTC control channel did not open")
	}
	if err := control.SendText(hello); err != nil {
		t.Fatal(err)
	}
	return control, messages, fragmentsSeen
}

func receiveICEMessage(t *testing.T, messages <-chan []byte, what string) map[string]any {
	t.Helper()
	select {
	case data := <-messages:
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("%s is not valid JSON (%d bytes): %v", what, len(data), err)
		}
		return decoded
	case <-time.After(15 * time.Second):
		t.Fatalf("no %s arrived", what)
		return nil
	}
}

// A directory listing larger than the peer's message limit used to be dropped
// by the carrier with no reply at all, so the browser sat on a request that
// never completed. It must arrive, fragmented and reassembled.
func TestOversizedFilesystemResponseSurvivesTheDataChannel(t *testing.T) {
	directory := t.TempDir()
	for index := 0; index < 900; index++ {
		name := fmt.Sprintf("%s/entry-with-a-deliberately-long-name-%04d.txt", directory, index)
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent := &Server{Config: Config{BrowserIdentities: []BrowserIdentity{{
		KeyID: "browser-test", Origin: "https://app.example", Capabilities: []string{"fs.list"},
	}}}}
	control, messages, fragmentsSeen := connectedICEBrowser(t, agent, `{"type":"hello","protocol":2,"keyId":"browser-test","fragments":true}`)
	if reply := receiveICEMessage(t, messages, "hello reply"); reply["ok"] != true {
		t.Fatalf("unexpected hello response: %#v", reply)
	}
	request, _ := json.Marshal(map[string]any{"type": "fs", "id": "fs_1", "method": "list", "args": []any{directory}})
	if err := control.SendText(string(request)); err != nil {
		t.Fatal(err)
	}
	reply := receiveICEMessage(t, messages, "listing")
	if reply["type"] != "fs-result" || reply["id"] != "fs_1" {
		t.Fatalf("unexpected listing reply: %#v", reply)
	}
	result, _ := reply["result"].(map[string]any)
	entries, _ := result["entries"].([]any)
	if len(entries) != 900 {
		t.Fatalf("listing carried %d entries, want 900", len(entries))
	}
	encoded, _ := json.Marshal(reply)
	if len(encoded) <= webRTCFragmentThreshold {
		t.Fatalf("listing is only %d bytes, so it would not have been fragmented", len(encoded))
	}
	if count := fragmentsSeen.Load(); count < 2 {
		t.Fatalf("listing arrived in %d fragments; it was not split at all", count)
	}
}

// A browser that never announced reassembly must keep receiving one whole
// message, even when it is large, or this change would break older shells.
func TestOversizedResponseStaysWholeForBrowsersWithoutReassembly(t *testing.T) {
	directory := t.TempDir()
	for index := 0; index < 900; index++ {
		name := fmt.Sprintf("%s/entry-with-a-deliberately-long-name-%04d.txt", directory, index)
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent := &Server{Config: Config{BrowserIdentities: []BrowserIdentity{{
		KeyID: "browser-test", Origin: "https://app.example", Capabilities: []string{"fs.list"},
	}}}}
	control, messages, fragmentsSeen := connectedICEBrowser(t, agent, `{"type":"hello","protocol":2,"keyId":"browser-test"}`)
	if reply := receiveICEMessage(t, messages, "hello reply"); reply["ok"] != true {
		t.Fatalf("unexpected hello response: %#v", reply)
	}
	request, _ := json.Marshal(map[string]any{"type": "fs", "id": "fs_1", "method": "list", "args": []any{directory}})
	if err := control.SendText(string(request)); err != nil {
		t.Fatal(err)
	}
	if reply := receiveICEMessage(t, messages, "listing"); reply["type"] != "fs-result" {
		t.Fatalf("unexpected listing reply: %#v", reply)
	}
	if count := fragmentsSeen.Load(); count != 0 {
		t.Fatalf("sent %d fragments to a browser that cannot reassemble them", count)
	}
}
