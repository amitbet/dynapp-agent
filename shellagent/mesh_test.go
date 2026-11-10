package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestSDPWithCandidateInsertsIntoFirstMediaSection(t *testing.T) {
	sdp := "v=0\r\no=- 1 2 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0\r\na=mid:0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.168.1.5 9012 typ host\r\na=end-of-candidates\r\na=sctp-port:5000\r\nm=audio 9 UDP 0\r\na=mid:1\r\n"
	line := "candidate:portmap 1 udp 1694498815 203.0.113.7 9012 typ srflx raddr 0.0.0.0 rport 9012"
	got := sdpWithCandidate(sdp, line)
	want := strings.Replace(sdp, "typ host\r\n", "typ host\r\na="+line+"\r\n", 1)
	if got != want {
		t.Fatalf("unexpected SDP:\n%q\nwant\n%q", got, want)
	}
	if again := sdpWithCandidate(got, strings.Replace(line, "portmap", "stun", 1)); again != got {
		t.Fatalf("a candidate for the same address must not be added twice:\n%q", again)
	}
	bare := "v=0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:data\r\n"
	if got := sdpWithCandidate(bare, line); got != bare+"a="+line+"\r\n" {
		t.Fatalf("unexpected SDP without candidates: %q", got)
	}
	if mid := sdpFirstMid(bare); mid != "data" {
		t.Fatalf("unexpected mid %q", mid)
	}
}

// udpForwarder plays the router: datagrams to its public address are
// forwarded to the agent's mapped local port, and replies go back out from
// the public address.
type udpForwarder struct {
	front     *net.UDPConn
	target    *net.UDPAddr
	forwarded atomic.Int64
	mu        sync.Mutex
	upstreams map[string]*net.UDPConn
}

func startUDPForwarder(t *testing.T, targetPort int) *udpForwarder {
	t.Helper()
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &udpForwarder{front: front, target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: targetPort}, upstreams: map[string]*net.UDPConn{}}
	t.Cleanup(func() {
		_ = front.Close()
		forwarder.mu.Lock()
		for _, upstream := range forwarder.upstreams {
			_ = upstream.Close()
		}
		forwarder.mu.Unlock()
	})
	go func() {
		buffer := make([]byte, 65536)
		for {
			count, from, err := front.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			forwarder.mu.Lock()
			upstream := forwarder.upstreams[from.String()]
			if upstream == nil {
				upstream, err = net.DialUDP("udp4", nil, forwarder.target)
				if err != nil {
					forwarder.mu.Unlock()
					continue
				}
				forwarder.upstreams[from.String()] = upstream
				go func(client *net.UDPAddr, upstream *net.UDPConn) {
					reply := make([]byte, 65536)
					for {
						count, err := upstream.Read(reply)
						if err != nil {
							return
						}
						_, _ = front.WriteToUDP(reply[:count], client)
					}
				}(from, upstream)
			}
			forwarder.mu.Unlock()
			forwarder.forwarded.Add(1)
			_, _ = upstream.Write(buffer[:count])
		}
	}()
	return forwarder
}

func (f *udpForwarder) address() netip.AddrPort {
	return f.front.LocalAddr().(*net.UDPAddr).AddrPort()
}

type meshBrowserOptions struct {
	// keepAnswerCandidate keeps only matching a=candidate lines of the answer
	// the browser sees; nil keeps all of them.
	keepAnswerCandidate func(line string) bool
	// stripOfferCandidates hides the browser's addresses from the agent, so
	// the agent can only learn them from inbound checks.
	stripOfferCandidates bool
	loopback             bool
}

type meshBrowser struct {
	peer       *webrtc.PeerConnection
	control    *webrtc.DataChannel
	messages   chan []byte
	answerSDP  string
	candidates func() [][]json.RawMessage
}

func filterSDPCandidates(sdp string, keep func(string) bool) string {
	lines := strings.Split(sdp, "\r\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "a=candidate:") && !keep(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\r\n")
}

// startMeshBrowser runs a pion "browser" peer through answerICESession
// against a fake Dyner. It returns errors instead of failing so several can
// run concurrently.
func startMeshBrowser(t *testing.T, agent *Server, sessionID, hello string, options meshBrowserOptions) (*meshBrowser, error) {
	answerPosted := make(chan struct{})
	var mu sync.Mutex
	var answer iceDescription
	var candidatePosts [][]json.RawMessage
	sessionPath := "/api/v1/remote-environments/env_test/ice/sessions/" + sessionID
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == sessionPath+"/answer":
			var posted iceDescription
			_ = json.NewDecoder(request.Body).Decode(&posted)
			mu.Lock()
			answer = posted
			mu.Unlock()
			close(answerPosted)
		case request.Method == http.MethodPost && request.URL.Path == sessionPath+"/shell-candidates":
			var list []json.RawMessage
			_ = json.NewDecoder(request.Body).Decode(&list)
			mu.Lock()
			candidatePosts = append(candidatePosts, list)
			mu.Unlock()
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": sessionID}})
	}))
	t.Cleanup(server.Close)

	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(options.loopback)
	peer, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = peer.Close() })
	control, err := peer.CreateDataChannel("dynapp-control-v2", &webrtc.DataChannelInit{Ordered: boolPointer(true)})
	if err != nil {
		return nil, err
	}
	opened := make(chan struct{})
	control.OnOpen(func() { close(opened) })
	messages := make(chan []byte, 8)
	control.OnMessage(func(message webrtc.DataChannelMessage) { messages <- append([]byte(nil), message.Data...) })
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		return nil, errors.New("browser ICE gathering timed out")
	}
	offerSDP := peer.LocalDescription().SDP
	if options.stripOfferCandidates {
		offerSDP = filterSDPCandidates(offerSDP, func(string) bool { return false })
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := agent.answerICESession(ctx, Config{
		DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: "device-secret",
	}, iceSession{ID: sessionID, Offer: &iceDescription{Type: "offer", SDP: offerSDP}}); err != nil {
		return nil, err
	}
	select {
	case <-answerPosted:
	case <-time.After(10 * time.Second):
		return nil, errors.New("agent did not post an ICE answer")
	}
	mu.Lock()
	remote := answer.SDP
	mu.Unlock()
	seen := remote
	if options.keepAnswerCandidate != nil {
		seen = filterSDPCandidates(remote, options.keepAnswerCandidate)
	}
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: seen}); err != nil {
		return nil, err
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("WebRTC control channel did not open (answer %q)", seen)
	}
	if err := control.SendText(hello); err != nil {
		return nil, err
	}
	return &meshBrowser{peer: peer, control: control, messages: messages, answerSDP: remote, candidates: func() [][]json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([][]json.RawMessage(nil), candidatePosts...)
	}}, nil
}

func meshTestAgent() *Server {
	return &Server{Config: Config{BrowserIdentities: []BrowserIdentity{{
		KeyID: "browser-test", Origin: "https://app.example", Capabilities: []string{"fs.list"},
	}}}}
}

const meshTestHello = `{"type":"hello","protocol":2,"keyId":"browser-test"}`

// With a router mapping, the answer advertises the mapped public address as
// a srflx candidate, and a browser that can only reach that address (the
// answer is cut down to it and the agent never sees the browser's own
// addresses) connects through the shared mux. Two sessions run at once.
func TestPortMappedAnswerConnectsThroughSharedMux(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	localPort := conn.LocalAddr().(*net.UDPAddr).Port
	mux := noLoopbackUDPMux{newICEUDPMux(conn)}
	t.Cleanup(func() { _ = mux.Close() })
	for _, address := range mux.GetListenAddresses() {
		if address.(*net.UDPAddr).IP.IsLoopback() {
			t.Fatalf("loopback leaked into the mux host addresses: %v", address)
		}
	}
	router := startUDPForwarder(t, localPort)
	external := router.address()
	agent := meshTestAgent()
	agent.iceTransport.Store(&iceMuxTransport{mux: mux, localPort: localPort, protocol: "pcp", external: external})

	wantLine := fmt.Sprintf("a=candidate:portmap 1 udp 1694498815 127.0.0.1 %d typ srflx raddr 0.0.0.0 rport %d", external.Port(), localPort)
	options := meshBrowserOptions{
		keepAnswerCandidate:  func(line string) bool { return line == wantLine },
		stripOfferCandidates: true,
		loopback:             true,
	}
	browsers := make([]*meshBrowser, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index := range browsers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			browsers[index], errs[index] = startMeshBrowser(t, agent, fmt.Sprintf("ice_portmapped%011d", index), meshTestHello, options)
		}()
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("browser %d: %v", index, err)
		}
	}
	for index, browser := range browsers {
		if !strings.Contains(browser.answerSDP, fmt.Sprintf(" %d typ host", localPort)) {
			t.Fatalf("browser %d: host candidates do not use the shared port %d:\n%s", index, localPort, browser.answerSDP)
		}
		if !strings.Contains(browser.answerSDP, wantLine+"\r\n") {
			t.Fatalf("browser %d: answer lacks the mapped candidate %q:\n%s", index, wantLine, browser.answerSDP)
		}
		reply := receiveICEMessage(t, browser.messages, "hello reply")
		if reply["ok"] != true {
			t.Fatalf("browser %d: unexpected hello reply %#v", index, reply)
		}
		transport, _ := reply["transport"].(map[string]any)
		mapping, _ := transport["portMapping"].(map[string]any)
		if mapping["protocol"] != "pcp" || mapping["external"] != external.String() || len(mapping) != 2 {
			t.Fatalf("browser %d: unexpected transport in hello reply: %#v", index, reply["transport"])
		}
		pair, err := browser.peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil {
			t.Fatalf("browser %d: no selected pair: %v", index, err)
		}
		if pair.Remote.Address != "127.0.0.1" || pair.Remote.Port != external.Port() || pair.Remote.Typ != webrtc.ICECandidateTypeSrflx {
			t.Fatalf("browser %d: connected over %s, not the mapped candidate", index, pair.Remote.String())
		}
		// The trickled list carries the mapped candidate too.
		found := false
		for _, list := range browser.candidates() {
			for _, raw := range list {
				var candidate webrtc.ICECandidateInit
				if json.Unmarshal(raw, &candidate) == nil && "a="+candidate.Candidate == wantLine && candidate.SDPMid != nil && *candidate.SDPMid == "0" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("browser %d: mapped candidate was not trickled: %v", index, browser.candidates())
		}
	}
	if router.forwarded.Load() == 0 {
		t.Fatal("no traffic went through the mapped address")
	}
	// Both sessions stay usable at the same time over the one socket.
	for index, browser := range browsers {
		request := fmt.Sprintf(`{"type":"ping","id":"p%d"}`, index)
		if err := browser.control.SendText(request); err != nil {
			t.Fatal(err)
		}
	}
	for index, browser := range browsers {
		if reply := receiveICEMessage(t, browser.messages, "pong"); reply["type"] != "pong" || reply["id"] != fmt.Sprintf("p%d", index) {
			t.Fatalf("browser %d: unexpected pong %#v", index, reply)
		}
	}
}

// Without a mapping the answer and hello reply are exactly as before.
func TestHelloReplyOmitsPortMappingWithoutMapping(t *testing.T) {
	agent := meshTestAgent()
	browser, err := startMeshBrowser(t, agent, "ice_unmappedsession0000", meshTestHello, meshBrowserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reply := receiveICEMessage(t, browser.messages, "hello reply")
	if reply["ok"] != true {
		t.Fatalf("unexpected hello reply %#v", reply)
	}
	if _, ok := reply["transport"]; ok {
		t.Fatalf("hello reply reports a transport without a mapping: %#v", reply)
	}
	if strings.Contains(browser.answerSDP, "portmap") {
		t.Fatalf("answer carries a mapped candidate without a mapping:\n%s", browser.answerSDP)
	}
}

func TestPortMappingHelloShape(t *testing.T) {
	var transport *iceMuxTransport
	if transport.helloPortMapping() != nil {
		t.Fatal("nil transport must not report a mapping")
	}
	transport = &iceMuxTransport{protocol: "natpmp", external: netip.MustParseAddrPort("198.51.100.4:9012")}
	wire, _ := json.Marshal(map[string]any{"transport": map[string]any{"portMapping": transport.helloPortMapping()}})
	if string(wire) != `{"transport":{"portMapping":{"external":"198.51.100.4:9012","protocol":"natpmp"}}}` {
		t.Fatalf("unexpected hello transport: %s", wire)
	}
}
