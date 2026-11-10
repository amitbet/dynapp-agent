package shellagent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/portmap"
	"github.com/pion/webrtc/v4"
	"nhooyr.io/websocket"
)

const inboxTestCredential = "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"

// fakeInbox is Dyner (ticket, browser-identities, ICE session routes) plus
// the relay worker's /v1/inbox socket.
type fakeInbox struct {
	t           *testing.T
	dyner       *httptest.Server
	worker      *httptest.Server
	tickets     atomic.Int64
	syncs       atomic.Int64
	connections atomic.Int64
	pings       atomic.Int64
	answers     atomic.Int64
	relayTicket atomic.Int64
	refuse      atomic.Bool // inbox-ticket answers 503
	mu          sync.Mutex
	sockets     []*websocket.Conn
	offer       string
	answered    map[string]bool
	connected   chan struct{}
	badAuth     atomic.Int64
}

func startFakeInbox(t *testing.T) *fakeInbox {
	t.Helper()
	inbox := &fakeInbox{t: t, connected: make(chan struct{}, 16), answered: map[string]bool{}}
	inbox.worker = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/inbox" || !strings.HasPrefix(request.URL.Query().Get("ticket"), "inbox-ticket-") {
			http.Error(response, "bad ticket", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(response, request, nil)
		if err != nil {
			return
		}
		inbox.connections.Add(1)
		inbox.mu.Lock()
		inbox.sockets = append(inbox.sockets, conn)
		inbox.mu.Unlock()
		inbox.connected <- struct{}{}
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if kind == websocket.MessageText && string(data) == "ping" {
				inbox.pings.Add(1)
				_ = conn.Write(context.Background(), websocket.MessageText, []byte("pong"))
			}
		}
	}))
	t.Cleanup(inbox.worker.Close)
	inbox.dyner = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "DynApp-Device "+inboxTestCredential {
			inbox.badAuth.Add(1)
		}
		const base = "/api/v1/remote-environments/env_test"
		path := request.URL.Path
		switch {
		case request.Method == http.MethodPost && path == base+"/inbox-ticket":
			if inbox.refuse.Load() {
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			count := inbox.tickets.Add(1)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"url":       "ws" + strings.TrimPrefix(inbox.worker.URL, "http") + "/v1/inbox",
				"ticket":    "inbox-ticket-" + string(rune('a'+count%26)),
				"expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			})
		case request.Method == http.MethodGet && path == base+"/browser-identities":
			inbox.syncs.Add(1)
			_ = json.NewEncoder(response).Encode(map[string]any{"browserIdentities": []any{}})
		case request.Method == http.MethodPost && path == base+"/ticket":
			inbox.relayTicket.Add(1)
			_ = json.NewEncoder(response).Encode(map[string]any{"connection": map[string]any{
				"endpoint": "ws://127.0.0.1:1/v1/connect", "ticket": "relay-ticket",
			}})
		case strings.HasPrefix(path, base+"/ice/sessions/"):
			rest := strings.TrimPrefix(path, base+"/ice/sessions/")
			id, suffix, _ := strings.Cut(rest, "/")
			inbox.mu.Lock()
			defer inbox.mu.Unlock()
			switch {
			case request.Method == http.MethodGet && suffix == "":
				session := map[string]any{"id": id, "offer": map[string]any{"type": "offer", "sdp": inbox.offer}}
				if inbox.answered[id] {
					session["answer"] = map[string]any{"type": "answer", "sdp": "v=0"}
				}
				_ = json.NewEncoder(response).Encode(map[string]any{"session": session})
			case request.Method == http.MethodPost && suffix == "answer":
				inbox.answers.Add(1)
				inbox.answered[id] = true
				_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": id}})
			default:
				_ = json.NewEncoder(response).Encode(map[string]any{"session": map[string]any{"id": id}})
			}
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(inbox.dyner.Close)
	return inbox
}

func (inbox *fakeInbox) push(t *testing.T, event map[string]any) {
	t.Helper()
	wire, _ := json.Marshal(event)
	inbox.mu.Lock()
	conn := inbox.sockets[len(inbox.sockets)-1]
	inbox.mu.Unlock()
	if err := conn.Write(context.Background(), websocket.MessageText, wire); err != nil {
		t.Fatal(err)
	}
}

func (inbox *fakeInbox) closeLatest(code websocket.StatusCode, reason string) {
	inbox.mu.Lock()
	conn := inbox.sockets[len(inbox.sockets)-1]
	inbox.mu.Unlock()
	_ = conn.Close(code, reason)
}

func (inbox *fakeInbox) waitConnected(t *testing.T) {
	t.Helper()
	select {
	case <-inbox.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect to the inbox")
	}
}

func inboxTestAgent(t *testing.T, inbox *fakeInbox, timing meshTiming) *Server {
	t.Helper()
	agent := &Server{
		Config: Config{
			SchemaVersion: ConfigSchemaVersion, DynerBaseURL: inbox.dyner.URL,
			EnvironmentID: "env_test", DeviceCredential: inboxTestCredential, RelayEnabled: true,
		},
		StateDir:           t.TempDir(),
		meshActive:         true,
		meshTimingOverride: &timing,
		// No router in tests: an ephemeral ICE port and no protocols.
		portmapOptions:   &portmap.Options{Protocols: []string{"none"}, RetryInterval: time.Hour},
		portmapLocalPort: -1,
	}
	t.Cleanup(func() {
		agent.mu.Lock()
		cancel := agent.identitySyncCancel
		agent.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		agent.stopMesh()
		agent.stopRelay()
		agent.stopICE()
	})
	return agent
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func browserOffer(t *testing.T) string {
	t.Helper()
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := peer.CreateDataChannel("dynapp-control-v2", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}

func TestInboxEventsTriggerActions(t *testing.T) {
	inbox := startFakeInbox(t)
	inbox.offer = browserOffer(t)
	// Long sync intervals: anything that happens here is the inbox's doing.
	agent := inboxTestAgent(t, inbox, meshTiming{sync: time.Hour, relaxedSync: time.Hour, ping: time.Hour})
	agent.startIdentitySync()
	inbox.waitConnected(t)
	waitFor(t, "inbox connected", agent.inboxConnected.Load)
	if inbox.badAuth.Load() != 0 {
		t.Fatal("a Dyner request lacked the device credential")
	}
	syncsBefore := inbox.syncs.Load()

	// ice-session: fetched and answered at once; a duplicate push (and the
	// session showing up again) answers nothing more.
	inbox.push(t, map[string]any{"type": "ice-session", "id": "ice_inboxsession00000001"})
	inbox.push(t, map[string]any{"type": "ice-session", "id": "ice_inboxsession00000001"})
	waitFor(t, "ICE answer", func() bool { return inbox.answers.Load() >= 1 })
	inbox.push(t, map[string]any{"type": "ice-session", "id": "ice_inboxsession00000001"})
	time.Sleep(300 * time.Millisecond)
	if got := inbox.answers.Load(); got != 1 {
		t.Fatalf("expected one answer for duplicate events, got %d", got)
	}
	if inbox.syncs.Load() != syncsBefore {
		t.Fatal("the ICE answer should not have needed a sync")
	}

	// relay-request opens the relay.
	inbox.push(t, map[string]any{"type": "relay-request"})
	waitFor(t, "relay to start", func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.relayCancel != nil
	})

	// identities-changed runs the sync now.
	inbox.push(t, map[string]any{"type": "identities-changed"})
	waitFor(t, "identity sync", func() bool { return inbox.syncs.Load() > syncsBefore })

	// Unknown events and garbage are ignored; the socket stays up.
	inbox.push(t, map[string]any{"type": "something-new"})
	time.Sleep(100 * time.Millisecond)
	if !agent.inboxConnected.Load() || inbox.connections.Load() != 1 {
		t.Fatal("unknown event disturbed the inbox")
	}
}

func TestInboxPingsAndReconnectsAfterServerClose(t *testing.T) {
	inbox := startFakeInbox(t)
	agent := inboxTestAgent(t, inbox, meshTiming{sync: time.Hour, relaxedSync: time.Hour, ping: 40 * time.Millisecond, backoffMin: 20 * time.Millisecond, backoffMax: 100 * time.Millisecond})
	agent.reconcileMesh()
	inbox.waitConnected(t)
	waitFor(t, "two pings", func() bool { return inbox.pings.Load() >= 2 })

	inbox.closeLatest(4001, "replaced")
	waitFor(t, "inbox down", func() bool { return !agent.inboxConnected.Load() })
	inbox.waitConnected(t)
	waitFor(t, "inbox reconnected", agent.inboxConnected.Load)
	if inbox.tickets.Load() != 2 || inbox.connections.Load() != 2 {
		t.Fatalf("expected a fresh ticket per connection: tickets=%d connections=%d", inbox.tickets.Load(), inbox.connections.Load())
	}

	// A refused ticket (relay disabled server-side) backs off and retries.
	inbox.refuse.Store(true)
	inbox.closeLatest(websocket.StatusGoingAway, "restart")
	time.Sleep(300 * time.Millisecond)
	if agent.inboxConnected.Load() {
		t.Fatal("inbox connected without a ticket")
	}
	inbox.refuse.Store(false)
	inbox.waitConnected(t)
	waitFor(t, "inbox reconnected after refusal", agent.inboxConnected.Load)
}

func TestInboxRevokeWaitsForSuccessfulSync(t *testing.T) {
	inbox := startFakeInbox(t)
	agent := inboxTestAgent(t, inbox, meshTiming{sync: time.Hour, relaxedSync: time.Hour, ping: time.Hour, backoffMin: 10 * time.Millisecond, backoffMax: 20 * time.Millisecond})
	agent.reconcileMesh()
	inbox.waitConnected(t)
	inbox.push(t, map[string]any{"type": "revoke"})
	waitFor(t, "inbox down", func() bool { return !agent.inboxConnected.Load() })
	// Reconciling again (as every sync attempt does) must not reconnect.
	agent.reconcileMesh()
	time.Sleep(200 * time.Millisecond)
	if inbox.connections.Load() != 1 {
		t.Fatalf("revoked inbox reconnected before a sync: %d connections", inbox.connections.Load())
	}
	agent.syncBrowserIdentitiesOnce(context.Background())
	inbox.waitConnected(t)
	waitFor(t, "inbox reconnected", agent.inboxConnected.Load)

	// The worker's own close code for a revoke counts the same.
	inbox.closeLatest(inboxCloseRevoked, "revoked")
	waitFor(t, "inbox down", func() bool { return !agent.inboxConnected.Load() })
	time.Sleep(200 * time.Millisecond)
	if inbox.connections.Load() != 2 {
		t.Fatalf("inbox reconnected after close 4003: %d connections", inbox.connections.Load())
	}
}

func TestInboxStopsWhenRemoteAccessTurnsOff(t *testing.T) {
	inbox := startFakeInbox(t)
	agent := inboxTestAgent(t, inbox, meshTiming{sync: time.Hour, relaxedSync: time.Hour, ping: time.Hour})
	agent.reconcileMesh()
	inbox.waitConnected(t)
	agent.mu.Lock()
	agent.Config.RelayEnabled = false
	agent.mu.Unlock()
	agent.reconcileMesh()
	waitFor(t, "inbox down", func() bool { return !agent.inboxConnected.Load() })
	agent.meshMu.Lock()
	running := agent.inboxCancel != nil || agent.portmapCancel != nil
	agent.meshMu.Unlock()
	if running {
		t.Fatal("inbox or port mapping still running with remote access off")
	}
}

func TestSyncIntervalRelaxesWhileInboxConnected(t *testing.T) {
	defaults := (&Server{}).timing()
	if defaults.sync != 5*time.Second || defaults.relaxedSync != 30*time.Second || defaults.ping != 30*time.Second || defaults.backoffMin != time.Second || defaults.backoffMax != 60*time.Second {
		t.Fatalf("unexpected default timing: %#v", defaults)
	}
	plain := &Server{}
	if got := plain.remoteSyncInterval(); got != 5*time.Second {
		t.Fatalf("interval while disconnected = %v", got)
	}
	plain.inboxConnected.Store(true)
	if got := plain.remoteSyncInterval(); got != 30*time.Second {
		t.Fatalf("interval while connected = %v", got)
	}

	inbox := startFakeInbox(t)
	inbox.refuse.Store(true)
	agent := inboxTestAgent(t, inbox, meshTiming{sync: 25 * time.Millisecond, relaxedSync: time.Hour, ping: time.Hour, backoffMin: 10 * time.Millisecond, backoffMax: 20 * time.Millisecond})
	agent.startIdentitySync()
	// Inbox down: the fast fallback sync runs.
	before := inbox.syncs.Load()
	time.Sleep(300 * time.Millisecond)
	if fast := inbox.syncs.Load() - before; fast < 4 {
		t.Fatalf("expected the 5 s-equivalent sync while the inbox is down, got %d syncs", fast)
	}
	// Inbox up: the sync relaxes.
	inbox.refuse.Store(false)
	inbox.waitConnected(t)
	waitFor(t, "inbox connected", agent.inboxConnected.Load)
	time.Sleep(60 * time.Millisecond) // let a sleep already in progress finish
	before = inbox.syncs.Load()
	time.Sleep(300 * time.Millisecond)
	if relaxed := inbox.syncs.Load() - before; relaxed > 0 {
		t.Fatalf("sync kept polling while the inbox was connected: %d syncs", relaxed)
	}
	// Inbox down again: an immediate catch-up sync, then the fast interval.
	inbox.refuse.Store(true)
	inbox.closeLatest(websocket.StatusGoingAway, "restart")
	before = inbox.syncs.Load()
	time.Sleep(300 * time.Millisecond)
	if fast := inbox.syncs.Load() - before; fast < 4 {
		t.Fatalf("sync did not return to the fast interval: %d syncs", fast)
	}
}

// fakePCPRouter answers PCP MAP requests with a public address and records
// each request's lifetime (0 = delete).
func fakePCPRouter(t *testing.T) (port int, lifetimes func() []uint32) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var mu sync.Mutex
	var seen []uint32
	go func() {
		buffer := make([]byte, 1100)
		for {
			count, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if count != 60 || buffer[0] != 2 || buffer[1] != 1 {
				continue
			}
			response := append([]byte(nil), buffer[:60]...)
			response[1] = 0x81
			response[3] = 0
			clear(response[8:24])
			lifetime := binary.BigEndian.Uint32(buffer[4:8])
			mu.Lock()
			seen = append(seen, lifetime)
			mu.Unlock()
			copy(response[42:44], buffer[40:42])
			external := netip.MustParseAddr("203.0.113.9").As16()
			copy(response[44:60], external[:])
			_, _ = conn.WriteToUDP(response, from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, func() []uint32 {
		mu.Lock()
		defer mu.Unlock()
		return append([]uint32(nil), seen...)
	}
}

func TestPortMappingFollowsRemoteAccess(t *testing.T) {
	inbox := startFakeInbox(t)
	agent := inboxTestAgent(t, inbox, meshTiming{sync: time.Hour, relaxedSync: time.Hour, ping: time.Hour})
	port, lifetimes := fakePCPRouter(t)
	agent.portmapOptions = &portmap.Options{
		Gateway: netip.MustParseAddr("127.0.0.1"), PCPPort: port, Protocols: []string{portmap.ProtocolPCP}, Timeout: time.Second,
	}
	if agent.portMappingStatus() != nil {
		t.Fatal("status reported before mapping started")
	}
	agent.reconcileMesh()
	waitFor(t, "advertised mapping", func() bool { return agent.currentICETransport() != nil })
	transport := agent.currentICETransport()
	if transport.protocol != "pcp" || transport.external.Addr() != netip.MustParseAddr("203.0.113.9") || transport.external.Port() != uint16(transport.localPort) {
		t.Fatalf("unexpected transport %#v", transport)
	}
	status, _ := agent.portMappingStatus().(portmap.Status)
	if status.State != "mapped" || status.Protocol != "pcp" || !status.Public {
		t.Fatalf("unexpected status %#v", agent.portMappingStatus())
	}
	wire, _ := json.Marshal(agent.portMappingStatus())
	if !strings.Contains(string(wire), `"state":"mapped"`) || !strings.Contains(string(wire), `"external":"203.0.113.9:`) {
		t.Fatalf("unexpected status JSON %s", wire)
	}

	// Remote access off: the mapping is deleted and no longer used.
	agent.mu.Lock()
	agent.Config.RelayEnabled = false
	agent.mu.Unlock()
	agent.reconcileMesh()
	if agent.currentICETransport() != nil || agent.portMappingStatus() != nil {
		t.Fatal("mapping still active with remote access off")
	}
	seen := lifetimes()
	if len(seen) < 2 || seen[0] != uint32(portmap.DefaultLifetime/time.Second) || seen[len(seen)-1] != 0 {
		t.Fatalf("expected map then delete, got lifetimes %v", seen)
	}
}
