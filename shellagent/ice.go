package shellagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"nhooyr.io/websocket"
)

type iceDescription struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type iceSession struct {
	ID                string            `json:"id"`
	Offer             *iceDescription   `json:"offer"`
	Answer            *iceDescription   `json:"answer"`
	BrowserCandidates []json.RawMessage `json:"browserCandidates"`
	Expires           string            `json:"expiresAt"`
}

// iceAnswerGatherWait bounds how long the answer waits for local candidate
// gathering. Server-reflexive candidates from STUN normally arrive within a
// few hundred milliseconds; anything later is trickled through Dyner.
const iceAnswerGatherWait = 1500 * time.Millisecond

// iceConnectWait bounds how long an answered session keeps exchanging trickled
// candidates before the peer is abandoned.
const iceConnectWait = 15 * time.Second

const iceCandidatePollInterval = 250 * time.Millisecond

// iceCandidateTrickler posts the agent's local ICE candidates to Dyner as they
// are gathered. Dyner stores an append-only list, so every post carries the
// full accumulated list and a trailing null marks the end of gathering.
type iceCandidateTrickler struct {
	mu        sync.Mutex
	gathered  []json.RawMessage
	posted    int
	ended     bool
	posting   bool
	post      func(list []json.RawMessage) error
	onFailure func(error)
}

func newICECandidateTrickler(post func(list []json.RawMessage) error) *iceCandidateTrickler {
	return &iceCandidateTrickler{post: post}
}

func (t *iceCandidateTrickler) add(candidate *webrtc.ICECandidate) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	if candidate == nil {
		t.ended = true
		t.gathered = append(t.gathered, json.RawMessage("null"))
	} else {
		encoded, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			t.mu.Unlock()
			return
		}
		t.gathered = append(t.gathered, encoded)
	}
	start := !t.posting
	t.posting = true
	t.mu.Unlock()
	if start {
		go t.drain()
	}
}

func (t *iceCandidateTrickler) drain() {
	for {
		t.mu.Lock()
		if t.posted >= len(t.gathered) {
			t.posting = false
			t.mu.Unlock()
			return
		}
		snapshot := append([]json.RawMessage(nil), t.gathered...)
		t.mu.Unlock()
		err := t.post(snapshot)
		t.mu.Lock()
		if err == nil {
			if len(snapshot) > t.posted {
				t.posted = len(snapshot)
			}
		} else {
			// Candidates only speed up connectivity checks; the answer already
			// carries everything gathered before it was posted.
			t.posting = false
			t.ended = true
			t.mu.Unlock()
			if t.onFailure != nil {
				t.onFailure(err)
			}
			return
		}
		t.mu.Unlock()
	}
}

// applyBrowserCandidates adds not-yet-seen browser candidates to the peer and
// reports whether the browser has signalled the end of its gathering.
func applyBrowserCandidates(peer *webrtc.PeerConnection, seen map[string]struct{}, list []json.RawMessage) (ended bool) {
	for _, raw := range list {
		key := string(raw)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		var candidate webrtc.ICECandidateInit
		if err := json.Unmarshal(raw, &candidate); err != nil || candidate.Candidate == "" {
			continue
		}
		_ = peer.AddICECandidate(candidate)
	}
	return false
}

// A data channel cannot carry a message larger than the size the peer
// advertised in its SDP. Chrome advertises 256 KiB, so a large directory
// listing or file read simply failed to send and the browser waited out its
// request timeout with no reply at all. Messages above this threshold are
// split into framed fragments the browser reassembles; the threshold sits
// below every implementation's limit, including pion's own 64 KiB default.
const webRTCFragmentThreshold = 60000

// webRTCMaxReceiveMessageSize is what this agent advertises it can receive, so
// the browser is not held to pion's much smaller default in its direction.
const webRTCMaxReceiveMessageSize = 1 << 20

// webRTCFragmentMagic marks a fragment of a larger message. A whole message is
// either JSON text or a DFB1 binary frame, so neither can collide with it.
const webRTCFragmentMagic = "DFRG"

const webRTCFragmentHeaderSize = 9

// webRTCFragments splits a payload into `DFRG` frames: the magic, a flag byte
// whose low bit marks text, then the fragment index and count as big-endian
// uint16s. The channel is ordered, so the browser reassembles in arrival order.
func webRTCFragments(payload []byte, text bool) [][]byte {
	count := (len(payload) + webRTCFragmentThreshold - 1) / webRTCFragmentThreshold
	if count == 0 || count > 0xffff {
		return nil
	}
	frames := make([][]byte, 0, count)
	for index := 0; index < count; index++ {
		end := min(len(payload), (index+1)*webRTCFragmentThreshold)
		chunk := payload[index*webRTCFragmentThreshold : end]
		frame := make([]byte, webRTCFragmentHeaderSize+len(chunk))
		copy(frame, webRTCFragmentMagic)
		if text {
			frame[4] = 1
		}
		binary.BigEndian.PutUint16(frame[5:7], uint16(index))
		binary.BigEndian.PutUint16(frame[7:9], uint16(count))
		copy(frame[webRTCFragmentHeaderSize:], chunk)
		frames = append(frames, frame)
	}
	return frames
}

type dataChannelMessage struct {
	kind websocket.MessageType
	data []byte
}

type webRTCChannelSocket struct {
	channel    *webrtc.DataChannel
	peer       *webrtc.PeerConnection
	closePeer  bool
	incoming   chan dataChannelMessage
	closed     chan struct{}
	closeOnce  sync.Once
	datagramMu sync.RWMutex
	datagram   *webrtc.DataChannel
	datagrams  chan []byte
	rawBytes   []byte
	// fragments is shared by every channel of one peer connection: the
	// browser announces reassembly support once, in its hello.
	fragments *atomic.Bool
}

func newWebRTCChannelSocket(channel *webrtc.DataChannel, peer *webrtc.PeerConnection, closePeer bool, fragments *atomic.Bool) *webRTCChannelSocket {
	if fragments == nil {
		fragments = &atomic.Bool{}
	}
	s := &webRTCChannelSocket{channel: channel, peer: peer, closePeer: closePeer, incoming: make(chan dataChannelMessage, 256), closed: make(chan struct{}), datagrams: make(chan []byte, 256), fragments: fragments}
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		kind := websocket.MessageBinary
		if message.IsString {
			kind = websocket.MessageText
		}
		data := append([]byte(nil), message.Data...)
		select {
		case s.incoming <- dataChannelMessage{kind: kind, data: data}:
		case <-s.closed:
		}
	})
	channel.OnClose(func() {
		s.closeOnce.Do(func() { close(s.closed) })
		if s.closePeer {
			_ = s.peer.Close()
		}
	})
	return s
}

func (s *webRTCChannelSocket) setDatagram(channel *webrtc.DataChannel) {
	s.datagramMu.Lock()
	s.datagram = channel
	s.datagramMu.Unlock()
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		data := append([]byte(nil), message.Data...)
		select {
		case s.datagrams <- data:
		case <-s.closed:
		}
	})
}

func (s *webRTCChannelSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case message := <-s.incoming:
		return message.kind, message.data, nil
	case <-s.closed:
		return 0, nil, errors.New("WebRTC channel closed")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (s *webRTCChannelSocket) Write(_ context.Context, kind websocket.MessageType, payload []byte) error {
	return s.writeMessage(payload, kind == websocket.MessageText)
}

// writeMessage sends one protocol message, fragmenting it when it would exceed
// what the peer accepts. Callers hold the per-connection write lock, so one
// message's fragments are never interleaved with another message.
func (s *webRTCChannelSocket) writeMessage(payload []byte, text bool) error {
	if len(payload) > webRTCFragmentThreshold && s.fragments.Load() {
		frames := webRTCFragments(payload, text)
		if frames == nil {
			return errors.New("WebRTC message is too large to fragment")
		}
		for _, frame := range frames {
			if err := s.channel.Send(frame); err != nil {
				return err
			}
		}
		return nil
	}
	if text {
		return s.channel.SendText(string(payload))
	}
	return s.channel.Send(payload)
}

func (s *webRTCChannelSocket) ReadRaw(ctx context.Context, payload []byte) (int, error) {
	if len(s.rawBytes) == 0 {
		_, next, err := s.Read(ctx)
		if err != nil {
			return 0, err
		}
		s.rawBytes = next
	}
	count := copy(payload, s.rawBytes)
	s.rawBytes = s.rawBytes[count:]
	return count, nil
}

func (s *webRTCChannelSocket) WriteRaw(_ context.Context, payload []byte) error {
	return s.writeMessage(payload, false)
}

func (s *webRTCChannelSocket) Close(websocket.StatusCode, string) error {
	var err error
	s.closeOnce.Do(func() { close(s.closed); err = s.channel.Close() })
	if s.closePeer {
		_ = s.peer.Close()
	}
	return err
}

func (s *webRTCChannelSocket) SendDatagram(payload []byte) error {
	s.datagramMu.RLock()
	channel := s.datagram
	s.datagramMu.RUnlock()
	if channel == nil || channel.ReadyState() != webrtc.DataChannelStateOpen {
		return errors.New("WebRTC datagram channel is not open")
	}
	return channel.Send(payload)
}

func (s *webRTCChannelSocket) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-s.datagrams:
		return payload, nil
	case <-s.closed:
		return nil, errors.New("WebRTC channel closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) startPendingICESessions(ctx context.Context, config Config, sessions []iceSession) {
	s.mu.Lock()
	current := s.Config.relayTicketConfig()
	if !current.RelayEnabled || current.EnvironmentID != config.EnvironmentID || current.DeviceCredential != config.DeviceCredential {
		s.mu.Unlock()
		return
	}
	if s.iceContext == nil {
		iceContext, cancel := context.WithCancel(ctx)
		s.iceContext = iceContext
		s.iceCancel = cancel
	}
	iceContext := s.iceContext
	s.mu.Unlock()
	for _, session := range sessions {
		if session.Offer == nil || session.Answer != nil {
			continue
		}
		s.iceMu.Lock()
		if s.activeICESessions == nil {
			s.activeICESessions = map[string]uint64{}
		}
		generation := s.iceGeneration
		_, exists := s.activeICESessions[session.ID]
		if !exists {
			s.activeICESessions[session.ID] = generation
		}
		s.iceMu.Unlock()
		if exists {
			continue
		}
		go func(item iceSession) {
			defer func() {
				s.iceMu.Lock()
				if s.activeICESessions[item.ID] == generation {
					delete(s.activeICESessions, item.ID)
				}
				s.iceMu.Unlock()
			}()
			if err := s.answerICESession(iceContext, config, item); err != nil && iceContext.Err() == nil {
				logICEError(item.ID, err)
			}
		}(session)
	}
}

func logICEError(id string, err error) {
	if !strings.Contains(err.Error(), "HTTP 409") {
		log.Printf("DynApp Shell agent: ICE session %s failed: %v", id, err)
	}
}

// authenticateICEHello retries the browser-key lookup once against Dyner when
// an ICE session reaches the agent before the periodic identity snapshot that
// contains its newly provisioned key. The signaling session proves account
// access, but the data channel still opens only for an exact browser grant.
func (s *Server) authenticateICEHello(ctx context.Context, config Config, hello message) socketAuthentication {
	grant := s.relayGrant(ctx, config, hello)
	if grant.keyID != "" || strings.TrimSpace(hello.KeyID) == "" {
		grant.ok = grant.keyID != ""
		return grant
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	state, err := SyncRemoteEnvironmentState(refreshCtx, nil, config)
	if err == nil {
		s.mu.Lock()
		s.Config.BrowserIdentities = mergeSyncedIdentities(s.Config.BrowserIdentities, state.BrowserIdentities)
		s.mu.Unlock()
		grant = s.relayGrant(ctx, config, hello)
	}
	grant.ok = grant.keyID != ""
	if !grant.ok {
		log.Printf("DynApp Shell agent: rejected ICE browser identity after refresh")
	}
	return grant
}

func (s *Server) answerICESession(ctx context.Context, config Config, session iceSession) error {
	// pion advertises a 64 KiB SCTP message limit by default, which caps what
	// the browser may send this agent. Announce a larger one; this agent's own
	// sends are capped by what the browser advertises, and are fragmented.
	settings := webrtc.SettingEngine{}
	settings.SetSCTPMaxMessageSize(webRTCMaxReceiveMessageSize)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	peer, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}}})
	if err != nil {
		return err
	}
	fragments := &atomic.Bool{}
	connectionCtx, cancel := context.WithCancel(ctx)
	connected := make(chan struct{})
	var connectedOnce sync.Once
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			cancel()
		}
	})
	channels := make(chan protocolSocket, 16)
	var controlMu sync.Mutex
	var control *webRTCChannelSocket
	var pendingDatagram *webrtc.DataChannel
	datagramReady := make(chan struct{})
	var datagramReadyOnce sync.Once
	var serveOnce sync.Once
	serveControl := func(socket *webRTCChannelSocket) {
		serveOnce.Do(func() {
			go func() {
				datagrams := false
				select {
				case <-datagramReady:
					datagrams = true
				case <-time.After(2 * time.Second):
				case <-connectionCtx.Done():
					return
				}
				s.serveAuthenticatedSocket(connectionCtx, socket, func(hello message) socketAuthentication {
					// Only a browser that says it reassembles fragments is
					// sent them; an older one still gets whole messages.
					if hello.Fragments {
						fragments.Store(true)
					}
					return s.authenticateICEHello(connectionCtx, config, hello)
				}, carrierCapabilities{
					reliableStreams: true, datagrams: datagrams, rawBridgeStreams: true, filesystemStreams: true, fragments: true,
				}, channels)
			}()
		})
	}
	peer.OnDataChannel(func(channel *webrtc.DataChannel) {
		switch {
		case channel.Label() == "dynapp-control-v2":
			socket := newWebRTCChannelSocket(channel, peer, true, fragments)
			controlMu.Lock()
			control = socket
			if pendingDatagram != nil {
				socket.setDatagram(pendingDatagram)
			}
			controlMu.Unlock()
			channel.OnOpen(func() { serveControl(socket) })
		case channel.Label() == "dynapp-datagram-v2":
			channel.OnOpen(func() {
				controlMu.Lock()
				socket := control
				if socket == nil {
					pendingDatagram = channel
				}
				controlMu.Unlock()
				if socket != nil {
					socket.setDatagram(channel)
				}
				datagramReadyOnce.Do(func() { close(datagramReady) })
			})
		case strings.HasPrefix(channel.Label(), "dynapp-stream-v2-"):
			socket := newWebRTCChannelSocket(channel, peer, false, fragments)
			channel.OnOpen(func() {
				select {
				case channels <- socket:
				case <-connectionCtx.Done():
				}
			})
		}
	})
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: session.Offer.SDP}
	if err := peer.SetRemoteDescription(offer); err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	seenBrowser := map[string]struct{}{}
	browserEnded := applyBrowserCandidates(peer, seenBrowser, session.BrowserCandidates)
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	// Trickle candidates gathered after the answer is posted. Candidates
	// gathered before that are already part of the answer SDP, but Dyner's
	// list is append-only, so the trickler still carries them.
	trickler := newICECandidateTrickler(func(list []json.RawMessage) error {
		return postICE(connectionCtx, nil, config, session.ID+"/shell-candidates", list)
	})
	peer.OnICECandidate(trickler.add)
	gather := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	select {
	case <-gather:
	case <-time.After(iceAnswerGatherWait):
	case <-ctx.Done():
		cancel()
		_ = peer.Close()
		return ctx.Err()
	}
	local := peer.LocalDescription()
	if local == nil {
		cancel()
		_ = peer.Close()
		return errors.New("WebRTC answer is unavailable")
	}
	if err := postICE(ctx, nil, config, session.ID+"/answer", iceDescription{Type: "answer", SDP: local.SDP}); err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	// The browser trickles its own candidates after posting the offer. Keep
	// pulling them until the peer connects, gathering ends, or we give up.
	go func() {
		for !browserEnded {
			select {
			case <-connected:
				return
			case <-connectionCtx.Done():
				return
			case <-time.After(iceCandidatePollInterval):
			}
			latest, err := fetchICESession(connectionCtx, nil, config, session.ID)
			if err != nil {
				continue
			}
			browserEnded = applyBrowserCandidates(peer, seenBrowser, latest.BrowserCandidates)
		}
	}()
	go func() {
		select {
		case <-connected:
			<-connectionCtx.Done()
		case <-time.After(iceConnectWait):
			cancel()
		case <-connectionCtx.Done():
		}
		_ = peer.Close()
	}()
	return nil
}

func iceURL(config Config, suffix string) (string, error) {
	base, err := url.Parse(config.DynerBaseURL)
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" + url.PathEscape(config.EnvironmentID) + "/ice/sessions" + suffix
	return base.String(), nil
}

func fetchICESession(ctx context.Context, client *http.Client, config Config, id string) (iceSession, error) {
	endpoint, err := iceURL(config, "/"+url.PathEscape(id))
	if err != nil {
		return iceSession{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return iceSession{}, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	response, err := client.Do(request)
	if err != nil {
		return iceSession{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return iceSession{}, fmt.Errorf("Dyner ICE session fetch failed: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Session iceSession `json:"session"`
	}
	err = json.NewDecoder(response.Body).Decode(&payload)
	return payload.Session, err
}

func postICE(ctx context.Context, client *http.Client, config Config, suffix string, body any) error {
	endpoint, err := iceURL(config, "/"+suffix)
	if err != nil {
		return err
	}
	wire, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("Dyner ICE update failed: HTTP %d", response.StatusCode)
	}
	return nil
}
