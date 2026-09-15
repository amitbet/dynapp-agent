package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestFetchICESessionsUsesDeviceAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/remote-environments/env_test/ice/sessions" {
			t.Fatalf("unexpected ICE request: %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "DynApp-Device device-secret" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{
			map[string]any{"id": "ice_abcdefghijklmnopqrstuv", "offer": map[string]any{"type": "offer", "sdp": "v=0"}},
		}})
	}))
	defer server.Close()

	sessions, err := fetchICESessions(context.Background(), server.Client(), Config{
		DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Offer == nil || sessions[0].Offer.Type != "offer" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
}

func TestAnswerICESessionEstablishesBrowserDataChannels(t *testing.T) {
	var answer iceDescription
	var answerMu sync.Mutex
	answerPosted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/remote-environments/env_test/ice/sessions/ice_abcdefghijklmnopqrstuv/answer" {
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
		if features["reliableStreams"] != true || features["datagrams"] != true || features["rawBridgeStreams"] != true {
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
}

func boolPointer(value bool) *bool { return &value }
