package linuxhost

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Distribution builds of WebKitGTK have no WebRTC, so a Linux app page could
// not punch through NATs or find a LAN peer by ICE, and fell back to the
// relay. The host stands in for the browser: the page gets an
// RTCPeerConnection whose peer connections and data channels pion runs here
// (docs/native-host.md, "Linux WebRTC"). Signaling is unchanged: the page
// still exchanges the offer, answer and candidates through Dyner.
//
// Only data channels exist; there are no media tracks.

//go:embed webrtc_polyfill.js
var webRTCPolyfill string

const (
	rtcMaxPeers         = 16
	rtcMaxChannels      = 512
	rtcMaxIDLength      = 64
	rtcMaxSDPLength     = 256 * 1024
	rtcMaxICEServers    = 16
	rtcMaxMessageBytes  = 1024 * 1024
	rtcMaxReceiveBytes  = 1024 * 1024
	rtcMaxLabelLength   = 256
	rtcMaxCandidateText = 2048
)

// RTCBridge serves the page's RTCPeerConnection stand-in.
type RTCBridge struct {
	// Deliver sends one event to the page.
	Deliver func(map[string]any)
	// NewAPI builds the pion API for a peer connection. Nil uses defaults
	// with a 1 MiB SCTP receive limit, like the agent's own answerer.
	NewAPI func() *webrtc.API

	mu     sync.Mutex
	peers  map[string]*rtcPeer
	closed bool
}

type rtcPeer struct {
	id       string
	pc       *webrtc.PeerConnection
	mu       sync.Mutex
	channels map[string]*webrtc.DataChannel
}

// RTCServer is one entry of the page's iceServers.
type RTCServer struct {
	URLs       json.RawMessage `json:"urls"`
	Username   string          `json:"username"`
	Credential string          `json:"credential"`
}

// Handle runs one rtc-* page message. Calls that pion answers at once run
// here; none waits on the network.
func (b *RTCBridge) Handle(message BridgeMessage) {
	if message.Op == "rtc-reset" {
		b.CloseAll()
		return
	}
	if message.PID == "" || len(message.PID) > rtcMaxIDLength || len(message.CID) > rtcMaxIDLength || len(message.RID) > rtcMaxIDLength {
		return
	}
	switch message.Op {
	case "rtc-new":
		b.newPeer(message)
	case "rtc-close":
		b.closePeer(message.PID)
	case "rtc-channel":
		b.createChannel(message)
	case "rtc-send":
		b.send(message)
	case "rtc-channel-close":
		if peer := b.peer(message.PID); peer != nil {
			if channel := peer.channel(message.CID); channel != nil {
				_ = channel.Close()
			}
		}
	case "rtc-offer", "rtc-local", "rtc-remote", "rtc-candidate", "rtc-stats":
		peer := b.peer(message.PID)
		if peer == nil {
			b.reply(message, nil, errors.New("RTCPeerConnection is closed"))
			return
		}
		// pion's calls are quick, but none of them should hold the GTK main
		// thread; each reply carries its request id.
		go func() {
			value, err := b.call(peer, message)
			b.reply(message, value, err)
		}()
	}
}

// CloseAll ends every peer connection, for a page reload.
func (b *RTCBridge) CloseAll() {
	b.mu.Lock()
	peers := b.peers
	b.peers = map[string]*rtcPeer{}
	b.mu.Unlock()
	for _, peer := range peers {
		go func(pc *webrtc.PeerConnection) { _ = pc.Close() }(peer.pc)
	}
}

// Shutdown ends every peer connection and refuses new ones.
func (b *RTCBridge) Shutdown() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.CloseAll()
}

func (b *RTCBridge) api() *webrtc.API {
	if b.NewAPI != nil {
		return b.NewAPI()
	}
	settings := webrtc.SettingEngine{}
	settings.SetSCTPMaxMessageSize(rtcMaxReceiveBytes)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

func (b *RTCBridge) newPeer(message BridgeMessage) {
	fail := func(err error) {
		b.Deliver(map[string]any{"pid": message.PID, "event": "failed", "error": err.Error()})
	}
	servers, err := iceServers(message.ICEServers)
	if err != nil {
		fail(err)
		return
	}
	b.mu.Lock()
	if b.peers == nil {
		b.peers = map[string]*rtcPeer{}
	}
	_, exists := b.peers[message.PID]
	switch {
	case b.closed:
		err = errors.New("the app window is closing")
	case exists:
		err = errors.New("duplicate RTCPeerConnection id")
	case len(b.peers) >= rtcMaxPeers:
		err = errors.New("too many RTCPeerConnections")
	}
	b.mu.Unlock()
	if err != nil {
		fail(err)
		return
	}
	pc, err := b.api().NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		fail(err)
		return
	}
	peer := &rtcPeer{id: message.PID, pc: pc, channels: map[string]*webrtc.DataChannel{}}
	b.mu.Lock()
	b.peers[message.PID] = peer
	b.mu.Unlock()
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		event := map[string]any{"pid": peer.id, "event": "icecandidate", "candidate": nil}
		if candidate != nil {
			event["candidate"] = candidate.ToJSON()
		}
		event["localDescription"] = description(pc.LocalDescription())
		b.Deliver(event)
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		b.Deliver(map[string]any{"pid": peer.id, "event": "icegatheringstatechange", "state": state.String(), "localDescription": description(pc.LocalDescription())})
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		b.Deliver(map[string]any{"pid": peer.id, "event": "iceconnectionstatechange", "state": state.String()})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		b.Deliver(map[string]any{"pid": peer.id, "event": "connectionstatechange", "state": state.String()})
		if state == webrtc.PeerConnectionStateClosed {
			b.forget(peer)
		}
	})
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		// Channels the far side opens get an id from the host.
		id := "remote-" + channel.Label()
		if channel.ID() != nil {
			id = "remote-" + strconv.Itoa(int(*channel.ID()))
		}
		if !peer.add(id, channel) {
			_ = channel.Close()
			return
		}
		b.Deliver(map[string]any{"pid": peer.id, "event": "datachannel", "cid": id, "label": channel.Label(), "ordered": channel.Ordered()})
		b.watchChannel(peer, id, channel)
	})
}

func iceServers(raw []RTCServer) ([]webrtc.ICEServer, error) {
	if len(raw) > rtcMaxICEServers {
		return nil, errors.New("too many iceServers")
	}
	servers := make([]webrtc.ICEServer, 0, len(raw))
	for _, server := range raw {
		var urls []string
		var single string
		if json.Unmarshal(server.URLs, &single) == nil && single != "" {
			urls = []string{single}
		} else if err := json.Unmarshal(server.URLs, &urls); err != nil || len(urls) == 0 {
			return nil, errors.New("each iceServer needs urls")
		}
		entry := webrtc.ICEServer{URLs: urls, Username: server.Username}
		if server.Credential != "" {
			entry.Credential = server.Credential
		}
		servers = append(servers, entry)
	}
	return servers, nil
}

func (b *RTCBridge) peer(id string) *rtcPeer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peers[id]
}

func (b *RTCBridge) forget(peer *rtcPeer) {
	b.mu.Lock()
	if b.peers[peer.id] == peer {
		delete(b.peers, peer.id)
	}
	b.mu.Unlock()
}

func (b *RTCBridge) closePeer(id string) {
	peer := b.peer(id)
	if peer == nil {
		return
	}
	b.forget(peer)
	go func() { _ = peer.pc.Close() }()
}

func (p *rtcPeer) add(id string, channel *webrtc.DataChannel) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.channels[id]; exists || len(p.channels) >= rtcMaxChannels {
		return false
	}
	p.channels[id] = channel
	return true
}

func (p *rtcPeer) channel(id string) *webrtc.DataChannel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channels[id]
}

func (p *rtcPeer) remove(id string) {
	p.mu.Lock()
	delete(p.channels, id)
	p.mu.Unlock()
}

func (b *RTCBridge) createChannel(message BridgeMessage) {
	peer := b.peer(message.PID)
	fail := func(err error) {
		b.Deliver(map[string]any{"pid": message.PID, "cid": message.CID, "event": "channel-error", "error": err.Error()})
	}
	if peer == nil {
		fail(errors.New("RTCPeerConnection is closed"))
		return
	}
	if message.CID == "" || len(message.Label) > rtcMaxLabelLength {
		fail(errors.New("invalid data channel"))
		return
	}
	init := &webrtc.DataChannelInit{}
	if message.Ordered != nil {
		ordered := *message.Ordered
		init.Ordered = &ordered
	}
	if message.MaxRetransmits != nil {
		retransmits := uint16(*message.MaxRetransmits)
		init.MaxRetransmits = &retransmits
	}
	channel, err := peer.pc.CreateDataChannel(message.Label, init)
	if err != nil {
		fail(err)
		return
	}
	if !peer.add(message.CID, channel) {
		_ = channel.Close()
		fail(errors.New("too many data channels"))
		return
	}
	b.watchChannel(peer, message.CID, channel)
}

func (b *RTCBridge) watchChannel(peer *rtcPeer, id string, channel *webrtc.DataChannel) {
	channel.OnOpen(func() {
		b.Deliver(map[string]any{"pid": peer.id, "cid": id, "event": "open"})
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		event := map[string]any{"pid": peer.id, "cid": id, "event": "message"}
		if message.IsString {
			event["text"] = string(message.Data)
		} else {
			event["binary"] = base64.StdEncoding.EncodeToString(message.Data)
		}
		b.Deliver(event)
	})
	channel.OnError(func(err error) {
		b.Deliver(map[string]any{"pid": peer.id, "cid": id, "event": "channel-error", "error": err.Error()})
	})
	channel.OnClose(func() {
		peer.remove(id)
		b.Deliver(map[string]any{"pid": peer.id, "cid": id, "event": "close"})
	})
}

func (b *RTCBridge) send(message BridgeMessage) {
	peer := b.peer(message.PID)
	if peer == nil {
		return
	}
	channel := peer.channel(message.CID)
	if channel == nil {
		return
	}
	var err error
	if message.Binary != "" || message.IsBinary {
		var data []byte
		data, err = base64.StdEncoding.DecodeString(message.Binary)
		if err == nil && len(data) > rtcMaxMessageBytes {
			err = errors.New("data channel message is too large")
		}
		if err == nil {
			err = channel.Send(data)
		}
	} else {
		if len(message.Text) > rtcMaxMessageBytes {
			err = errors.New("data channel message is too large")
		} else {
			err = channel.SendText(message.Text)
		}
	}
	if err != nil {
		b.Deliver(map[string]any{"pid": peer.id, "cid": message.CID, "event": "channel-error", "error": err.Error()})
	}
}

// call runs one request that the page awaits.
func (b *RTCBridge) call(peer *rtcPeer, message BridgeMessage) (any, error) {
	switch message.Op {
	case "rtc-offer":
		offer, err := peer.pc.CreateOffer(nil)
		if err != nil {
			return nil, err
		}
		return description(&offer), nil
	case "rtc-local", "rtc-remote":
		var desc webrtc.SessionDescription
		if len(message.SDP) > rtcMaxSDPLength {
			return nil, errors.New("session description is too large")
		}
		if message.Op == "rtc-local" && message.SDPType == "" {
			offer, err := peer.pc.CreateOffer(nil)
			if err != nil {
				return nil, err
			}
			desc = offer
		} else {
			desc = webrtc.SessionDescription{Type: webrtc.NewSDPType(message.SDPType), SDP: message.SDP}
			if desc.Type == webrtc.SDPTypeUnknown {
				return nil, errors.New("unknown session description type")
			}
		}
		var err error
		if message.Op == "rtc-local" {
			err = peer.pc.SetLocalDescription(desc)
		} else {
			err = peer.pc.SetRemoteDescription(desc)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"localDescription": description(peer.pc.LocalDescription()), "remoteDescription": description(peer.pc.RemoteDescription())}, nil
	case "rtc-candidate":
		var candidate webrtc.ICECandidateInit
		if len(message.Candidate) > rtcMaxCandidateText {
			return nil, errors.New("ICE candidate is too large")
		}
		if err := json.Unmarshal(message.Candidate, &candidate); err != nil {
			return nil, err
		}
		if candidate.Candidate == "" {
			return nil, nil // end of candidates
		}
		return nil, peer.pc.AddICECandidate(candidate)
	case "rtc-stats":
		stats := []any{}
		for _, stat := range peer.pc.GetStats() {
			stats = append(stats, stat)
		}
		return stats, nil
	}
	return nil, errors.New("unknown request")
}

func (b *RTCBridge) reply(message BridgeMessage, value any, err error) {
	event := map[string]any{"pid": message.PID, "rid": message.RID, "event": "result"}
	if err != nil {
		event["error"] = err.Error()
	} else {
		event["value"] = value
	}
	b.Deliver(event)
}

func description(desc *webrtc.SessionDescription) any {
	if desc == nil {
		return nil
	}
	return map[string]any{"type": desc.Type.String(), "sdp": desc.SDP}
}
