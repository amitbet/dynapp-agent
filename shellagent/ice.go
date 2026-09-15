package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"nhooyr.io/websocket"
)

const icePollInterval = time.Second

type iceDescription struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type iceSession struct {
	ID      string          `json:"id"`
	Offer   *iceDescription `json:"offer"`
	Answer  *iceDescription `json:"answer"`
	Expires string          `json:"expiresAt"`
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
}

func newWebRTCChannelSocket(channel *webrtc.DataChannel, peer *webrtc.PeerConnection, closePeer bool) *webRTCChannelSocket {
	s := &webRTCChannelSocket{channel: channel, peer: peer, closePeer: closePeer, incoming: make(chan dataChannelMessage, 256), closed: make(chan struct{}), datagrams: make(chan []byte, 256)}
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
	if kind == websocket.MessageText {
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
	return s.channel.Send(payload)
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

func (s *Server) runICE(ctx context.Context, config Config) {
	active := map[string]struct{}{}
	var mu sync.Mutex
	var lastIdentitySync time.Time
	for ctx.Err() == nil {
		if time.Since(lastIdentitySync) >= 10*time.Second {
			if identities, err := SyncBrowserIdentities(ctx, nil, config); err == nil {
				s.mu.Lock()
				s.Config.BrowserIdentities = mergeSyncedIdentities(s.Config.BrowserIdentities, identities)
				s.mu.Unlock()
			}
			lastIdentitySync = time.Now()
		}
		sessions, err := fetchICESessions(ctx, nil, config)
		if err == nil {
			for _, session := range sessions {
				if session.Offer == nil || session.Answer != nil {
					continue
				}
				mu.Lock()
				_, exists := active[session.ID]
				if !exists {
					active[session.ID] = struct{}{}
				}
				mu.Unlock()
				if exists {
					continue
				}
				go func(item iceSession) {
					defer func() { mu.Lock(); delete(active, item.ID); mu.Unlock() }()
					if err := s.answerICESession(ctx, config, item); err != nil && ctx.Err() == nil {
						logICEError(item.ID, err)
					}
				}(session)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(icePollInterval):
		}
	}
}

func logICEError(id string, err error) {
	if !strings.Contains(err.Error(), "HTTP 409") {
		log.Printf("DynApp Shell agent: ICE session %s failed: %v", id, err)
	}
}

func (s *Server) answerICESession(ctx context.Context, config Config, session iceSession) error {
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}}})
	if err != nil {
		return err
	}
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
					grant := s.relayGrant(connectionCtx, config, hello)
					// ICE signaling proves account access to the environment. The
					// data channel must still name an exact, synced browser grant.
					grant.ok = grant.keyID != ""
					return grant
				}, carrierCapabilities{
					reliableStreams: true, datagrams: datagrams, rawBridgeStreams: true, filesystemStreams: true,
				}, channels)
			}()
		})
	}
	peer.OnDataChannel(func(channel *webrtc.DataChannel) {
		switch {
		case channel.Label() == "dynapp-control-v2":
			socket := newWebRTCChannelSocket(channel, peer, true)
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
			socket := newWebRTCChannelSocket(channel, peer, false)
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
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	gather := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		cancel()
		_ = peer.Close()
		return err
	}
	select {
	case <-gather:
	case <-time.After(3 * time.Second):
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
	go func() {
		select {
		case <-connected:
			<-connectionCtx.Done()
		case <-time.After(15 * time.Second):
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

func fetchICESessions(ctx context.Context, client *http.Client, config Config) ([]iceSession, error) {
	endpoint, err := iceURL(config, "")
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Dyner ICE poll failed: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Sessions []iceSession `json:"sessions"`
	}
	err = json.NewDecoder(response.Body).Decode(&payload)
	return payload.Sessions, err
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
