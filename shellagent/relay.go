package shellagent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const relayReconnectDelay = 5 * time.Second

// runRelay keeps one device-authenticated outbound connection alive. The
// Cloudflare relay owns browser multiplexing; each browser record supplies a
// session id which resets local authentication and bridge ownership.
func (s *Server) runRelay(ctx context.Context, config Config) {
	for ctx.Err() == nil {
		connection, err := IssueRelayTicket(ctx, nil, config)
		if err == nil {
			if identities, normalizeErr := combineDynerIdentities(connection.BrowserIdentities, connection.SharedIdentities); normalizeErr == nil {
				s.mu.Lock()
				s.Config.BrowserIdentities = identities
				config.BrowserIdentities = identities
				snapshot := s.Config
				s.mu.Unlock()
				_ = SaveConfig(s.StateDir, snapshot)
			}
			endpoint, parseErr := url.Parse(connection.Endpoint)
			if parseErr == nil {
				query := endpoint.Query()
				query.Set("ticket", connection.Ticket)
				endpoint.RawQuery = query.Encode()
				var socket *websocket.Conn
				socket, _, err = websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPClient: &http.Client{Timeout: 20 * time.Second}})
				if err == nil {
					s.handleRelaySocket(ctx, socket, connection, config)
					_ = socket.Close(websocket.StatusNormalClosure, "")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(relayReconnectDelay):
		}
	}
}

// relayRetiredLimit bounds how many ended browser session ids the relay
// remembers, so late frames from a replaced browser socket are dropped.
const relayRetiredLimit = 256

// relayHub demultiplexes the one agent socket to the relay Durable Object.
// The DO tags each browser frame with that browser socket's ticket id and
// forwards agent frames, untagged, to its single current browser socket. So
// each id becomes a virtual socket served by the shared session core, and a
// frame under a new id means the DO replaced the browser socket: every
// earlier virtual socket's carrier is dead, and only the current one may
// write.
type relayHub struct {
	socket protocolSocket
	ctx    context.Context
	// writeMu serializes writes on the shared socket and the switch of the
	// current id, so a replaced session never writes to its successor.
	writeMu sync.Mutex
	current *relayVirtualSocket
	retired map[string]struct{}
	order   []string
}

func newRelayHub(ctx context.Context, socket protocolSocket) *relayHub {
	return &relayHub{socket: socket, ctx: ctx, retired: map[string]struct{}{}}
}

// route returns the virtual socket for a browser session id, creating it
// when the id is new. created reports a socket that still needs serving.
func (hub *relayHub) route(id string, e2ee bool) (socket *relayVirtualSocket, created bool) {
	hub.writeMu.Lock()
	if hub.current != nil && hub.current.id == id {
		socket = hub.current
		hub.writeMu.Unlock()
		return socket, false
	}
	if _, ok := hub.retired[id]; ok {
		hub.writeMu.Unlock()
		return nil, false
	}
	previous := hub.current
	socket = &relayVirtualSocket{hub: hub, id: id, e2eeRequired: e2ee, incoming: make(chan dataChannelMessage, 256), closed: make(chan struct{})}
	hub.current = socket
	if previous != nil {
		hub.retireLocked(previous.id)
	}
	hub.writeMu.Unlock()
	if previous != nil {
		previous.markDead()
	}
	return socket, true
}

func (hub *relayHub) retireLocked(id string) {
	if _, ok := hub.retired[id]; ok {
		return
	}
	hub.retired[id] = struct{}{}
	hub.order = append(hub.order, id)
	if len(hub.order) > relayRetiredLimit {
		delete(hub.retired, hub.order[0])
		hub.order = hub.order[1:]
	}
}

func (hub *relayHub) closeAll() {
	hub.writeMu.Lock()
	current := hub.current
	hub.current = nil
	hub.writeMu.Unlock()
	if current != nil {
		current.markDead()
	}
}

// relayVirtualSocket is one browser session on the relay socket. Legacy
// end-to-end encryption is negotiated inside it, before the hello.
type relayVirtualSocket struct {
	hub          *relayHub
	id           string
	incoming     chan dataChannelMessage
	closed       chan struct{}
	closeOnce    sync.Once
	e2eeRequired bool
	accepted     map[string]bool
	tokenMu      sync.Mutex
	e2eeToken    string
}

func (v *relayVirtualSocket) markDead() {
	v.closeOnce.Do(func() {
		close(v.closed)
		connectionE2EE.Delete(v)
	})
}

func (v *relayVirtualSocket) token() string {
	v.tokenMu.Lock()
	defer v.tokenMu.Unlock()
	return v.e2eeToken
}

func (v *relayVirtualSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	for {
		var frame dataChannelMessage
		select {
		case frame = <-v.incoming:
		case <-v.closed:
			return 0, nil, errors.New("relay browser session ended")
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
		if !v.e2eeRequired {
			return frame.kind, frame.data, nil
		}
		token := v.token()
		if token == "" {
			v.negotiateE2EE(ctx, frame.data)
			continue
		}
		plain, err := openRemoteFrame(token, frame.data)
		if err != nil {
			continue
		}
		kind := websocket.MessageText
		if len(plain) >= 4 && string(plain[:4]) == "DFB1" {
			kind = websocket.MessageBinary
		}
		return kind, plain, nil
	}
}

func (v *relayVirtualSocket) negotiateE2EE(ctx context.Context, data []byte) {
	var init struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		KeyID   string `json:"keyId"`
	}
	if json.Unmarshal(data, &init) != nil || init.Type != "dynapp-e2ee-init" || init.Version != 1 {
		_ = v.writeFrame(ctx, map[string]any{"type": "error", "error": "End-to-end encryption is required"})
		return
	}
	token := ""
	for candidate := range v.accepted {
		if remoteKeyID(candidate) == init.KeyID {
			token = candidate
			break
		}
	}
	if token == "" {
		_ = v.writeFrame(ctx, map[string]any{"type": "error", "error": "End-to-end encryption credential rejected"})
		return
	}
	// Ready is intentionally outside the protected stream, matching the
	// existing Electron implementation.
	_ = v.writeFrame(ctx, map[string]any{"type": "dynapp-e2ee-ready", "version": 1})
	v.tokenMu.Lock()
	v.e2eeToken = token
	v.tokenMu.Unlock()
	connectionE2EE.Store(v, relayCipher{tokenHash: token})
}

// writeFrame sends a JSON record outside send(), which keeps a per-socket
// writer entry this socket may no longer have; it seals the record once
// encryption is up.
func (v *relayVirtualSocket) writeFrame(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if token := v.token(); token != "" {
		if data, err = sealRemoteFrame(token, data); err != nil {
			return err
		}
	}
	return v.Write(ctx, websocket.MessageText, data)
}

func (v *relayVirtualSocket) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	v.hub.writeMu.Lock()
	defer v.hub.writeMu.Unlock()
	if v.hub.current != v {
		return errors.New("relay browser session ended")
	}
	return v.hub.socket.Write(ctx, kind, data)
}

// Close cannot close the browser's relay socket; it reports the close as an
// `error` frame (the relay's existing rejection record, plus the code) while
// this session still owns the browser socket, then drops the session id.
func (v *relayVirtualSocket) Close(code websocket.StatusCode, reason string) error {
	_ = v.writeFrame(v.hub.ctx, map[string]any{"type": "error", "error": reason, "code": int(code)})
	v.hub.writeMu.Lock()
	if v.hub.current == v {
		v.hub.current = nil
	}
	v.hub.retireLocked(v.id)
	v.hub.writeMu.Unlock()
	v.markDead()
	return nil
}

func (s *Server) handleRelaySocket(ctx context.Context, socket *websocket.Conn, relay RelayConnection, config Config) {
	socket.SetReadLimit(maxReadBytes + 512*1024)
	accepted, subjects := relayAcceptedAccess(relay)
	e2ee := relay.Transport.E2EEncryption
	hub := newRelayHub(ctx, socket)
	defer hub.closeAll()
	authenticate := func(hello message) socketAuthentication {
		candidate := normalizeAuthHash(hello.TokenHash)
		if candidate == "" {
			candidate = normalizeAuthHash(hello.Token)
		}
		if !accepted[candidate] {
			return socketAuthentication{}
		}
		granted := s.relayGrant(ctx, config, hello, subjects[candidate])
		granted.environment = map[string]any{"id": config.EnvironmentID, "name": "This device", "endpoint": nil, "state": "connected", "source": "relay", "capabilities": granted.capabilities}
		return granted
	}
	carrier := carrierCapabilities{resumable: !e2ee, rejectReason: "Pairing credential rejected"}
	for ctx.Err() == nil {
		kind, wire, err := socket.Read(ctx)
		if err != nil {
			return
		}
		incoming, id, ok := decodeRelayFrame(kind, wire)
		if !ok {
			continue
		}
		s.noteRelayActivity()
		virtual, created := hub.route(id, e2ee)
		if virtual == nil {
			continue
		}
		if created {
			virtual.accepted = accepted
			go s.serveAuthenticatedSocket(ctx, virtual, authenticate, carrier, nil)
		}
		frameKind := websocket.MessageText
		if kind == websocket.MessageBinary {
			frameKind = websocket.MessageBinary
		}
		select {
		case virtual.incoming <- dataChannelMessage{kind: frameKind, data: incoming}:
		case <-virtual.closed:
		case <-ctx.Done():
			return
		}
	}
}

// relayGrant computes a relay socket's capabilities: the synced browser
// identity's grant intersected with the manifest of the app the hello names.
// Without an app block (or when Dyner cannot resolve it) the identity grant
// alone applies. A hello that names no known identity is limited to what
// every synced identity holds, so it never exceeds any authority decision.
func (s *Server) relayGrant(ctx context.Context, config Config, hello message, subject sessionSubject) socketAuthentication {
	s.mu.Lock()
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	return relayGrantFor(identities, hello, subject, func(storeID string) (resolvedApp, bool) {
		resolved, err := s.resolveManifestApp(ctx, config, storeID)
		return resolved, err == nil
	})
}

// relayGrantFor binds a hello to the identities its key id names, subject to
// the session subject (see identityMatchesSubject). A key that names shared
// identities yields a socket scoped to their targets. A hello that names no
// identity falls back to the intersection of every non-shared identity's
// grant, except for a shared subject, which gets nothing.
func relayGrantFor(identities []BrowserIdentity, hello message, subject sessionSubject, resolve func(storeID string) (resolvedApp, bool)) socketAuthentication {
	var base []string
	keyID := strings.TrimSpace(hello.KeyID)
	var candidates []BrowserIdentity
	if keyID != "" {
		for _, identity := range identities {
			if identity.KeyID != keyID || identity.Authority {
				continue
			}
			if identity.StoreID != "" && hello.App != nil && identity.StoreID != strings.TrimSpace(hello.App.StoreID) {
				continue
			}
			if !identityMatchesSubject(identity, subject) {
				continue
			}
			candidates = append(candidates, identity)
		}
	}
	// A key that is both shared and not (never expected) is treated as
	// shared: the narrower grant wins.
	shared := false
	for _, identity := range candidates {
		if identity.Shared {
			shared = true
			break
		}
	}
	matched := false
	var matchedOrigin string
	var matchedCapabilities []string
	var targets []string
	connect := map[string][]string{}
	var serverAPIs []ServerAPI
	for _, identity := range candidates {
		if shared && !identity.Shared {
			continue
		}
		base = unionStrings(base, identity.Capabilities)
		if matchedOrigin == "" {
			matchedOrigin = identity.Origin
			matchedCapabilities = append([]string(nil), identity.Capabilities...)
		}
		for scheme, patterns := range identity.Connect {
			connect[scheme] = unionStrings(connect[scheme], patterns)
		}
		targets = unionTargets(targets, identity.Targets)
		serverAPIs = unionServerAPIs(serverAPIs, identity.ServerAPIs)
		matched = true
	}
	if !matched {
		// Without a bound identity the HTTP handler keeps honoring the
		// request's own connect list, as before.
		keyID = ""
	}
	if !matched && !subject.Shared {
		started := false
		for _, identity := range identities {
			if identity.Authority || identity.Shared {
				continue
			}
			if !started {
				base = normalizePermissionSet(identity.Capabilities)
				started = true
				continue
			}
			base = intersectStrings(base, identity.Capabilities)
		}
	}
	auth := socketAuthentication{capabilities: normalizePermissionSet(base), keyID: keyID, origin: matchedOrigin, stored: normalizePermissionSet(matchedCapabilities), ok: true}
	auth.declared = auth.capabilities
	if matched {
		auth.connect = connect
		auth.serverAPIs = serverAPIs
	}
	if shared || (!matched && subject.Shared) {
		// A grantee reaches only its targets; without a matching identity it
		// is scoped to nothing at all.
		auth.scoped = true
		auth.targets = targets
		auth.subject = subject.UserID
	}
	if hello.App != nil {
		if storeID := strings.TrimSpace(hello.App.StoreID); validStoreID(storeID) {
			auth.storeID = storeID
			auth.appName = storeSlug(storeID)
			if resolved, ok := resolve(storeID); ok {
				auth.capabilities = normalizePermissionSet(intersectStrings(auth.capabilities, resolved.Declared))
				auth.declared = resolved.Declared
				auth.connect = resolved.Connect
				auth.serverAPIs, auth.serverProbePorts = resolved.ServerAPIs, resolved.ServerProbePorts
				auth.appName = resolved.Name
			}
		}
	}
	return auth
}

func decodeRelayFrame(kind websocket.MessageType, wire []byte) ([]byte, string, bool) {
	if kind == websocket.MessageBinary && len(wire) >= 6 && string(wire[:4]) == "DRL1" {
		length := int(binary.BigEndian.Uint16(wire[4:6]))
		if length < 1 || 6+length > len(wire) {
			return nil, "", false
		}
		return wire[6+length:], string(wire[6 : 6+length]), true
	}
	var envelope struct{ Type, Session, Payload string }
	if json.Unmarshal(wire, &envelope) != nil || envelope.Type != "dynapp-relay-frame" || envelope.Session == "" {
		return nil, "", false
	}
	return []byte(envelope.Payload), envelope.Session, true
}

func normalizeAuthHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 64 {
		for _, char := range value {
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return ""
			}
		}
		return strings.ToLower(value)
	}
	if len(value) >= 40 {
		sum := sha256.Sum256([]byte(value))
		return fmt.Sprintf("%x", sum[:])
	}
	return ""
}
