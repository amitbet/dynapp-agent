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
			if identities, normalizeErr := normalizeBrowserIdentities(connection.BrowserIdentities); normalizeErr == nil {
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

func (s *Server) handleRelaySocket(ctx context.Context, socket *websocket.Conn, relay RelayConnection, config Config) {
	connectionWriters.Store(socket, &sync.Mutex{})
	defer connectionWriters.Delete(socket)
	defer connectionE2EE.Delete(socket)
	socket.SetReadLimit(maxReadBytes + 512*1024)
	accepted := make(map[string]bool, len(relay.AcceptedAuthTokenHashes))
	for _, hash := range relay.AcceptedAuthTokenHashes {
		accepted[strings.ToLower(hash)] = true
	}
	var session string
	authenticated := false
	var negotiated int
	var granted socketAuthentication
	bridges := newBridgeSet(&s.activeBridges)
	defer bridges.closeAll()
	processes := newProcessSet(socket, ctx)
	defer processes.closeAll()
	agents := newAgentService(s, socket)
	defer agents.closeAll()
	e2eeRequired := relay.Transport.E2EEncryption
	var e2eeToken string
	reset := func(next string) {
		if session == next {
			return
		}
		bridges.closeAll()
		processes.closeAll()
		bridges = newBridgeSet()
		session, authenticated, negotiated, e2eeToken = next, false, 0, ""
		granted = socketAuthentication{}
		connectionE2EE.Delete(socket)
	}
	for ctx.Err() == nil {
		kind, wire, err := socket.Read(ctx)
		if err != nil {
			return
		}
		incoming, nextSession, ok := decodeRelayFrame(kind, wire)
		if !ok {
			continue
		}
		reset(nextSession)
		if e2eeRequired {
			if e2eeToken == "" {
				var init struct {
					Type    string `json:"type"`
					Version int    `json:"version"`
					KeyID   string `json:"keyId"`
				}
				if json.Unmarshal(incoming, &init) != nil || init.Type != "dynapp-e2ee-init" || init.Version != 1 {
					send(socket, ctx, map[string]any{"type": "error", "error": "End-to-end encryption is required"})
					continue
				}
				for candidate := range accepted {
					if remoteKeyID(candidate) == init.KeyID {
						e2eeToken = candidate
						break
					}
				}
				if e2eeToken == "" {
					send(socket, ctx, map[string]any{"type": "error", "error": "End-to-end encryption credential rejected"})
					continue
				}
				// Ready is intentionally outside the protected stream, matching the
				// existing Electron implementation.
				connectionE2EE.Delete(socket)
				send(socket, ctx, map[string]any{"type": "dynapp-e2ee-ready", "version": 1})
				connectionE2EE.Store(socket, relayCipher{tokenHash: e2eeToken})
				continue
			}
			incoming, err = openRemoteFrame(e2eeToken, incoming)
			if err != nil {
				continue
			}
		}
		var request message
		var body []byte
		if len(incoming) >= 4 && string(incoming[:4]) == "DFB1" {
			request, body, err = decodeBinaryFrame(incoming)
		} else {
			err = json.Unmarshal(incoming, &request)
		}
		if err != nil {
			sendError(socket, ctx, "", "Remote message is invalid")
			continue
		}
		if !authenticated {
			candidate := normalizeAuthHash(request.TokenHash)
			if candidate == "" {
				candidate = normalizeAuthHash(request.Token)
			}
			version := protocolNumber(request.Protocol)
			if request.Type != "hello" || (version != LegacyProtocolVersion && version != ProtocolVersion) || !accepted[candidate] {
				send(socket, ctx, map[string]any{"type": "error", "error": "Pairing credential rejected"})
				continue
			}
			authenticated, negotiated = true, version
			granted = s.relayGrant(ctx, config, request)
			send(socket, ctx, map[string]any{"type": "hello", "ok": true, "protocol": negotiated, "serverId": "go-shell-agent", "environment": map[string]any{"id": config.EnvironmentID, "name": "This device", "endpoint": nil, "state": "connected", "source": "relay", "capabilities": granted.capabilities}, "capabilities": granted.capabilities, "features": protocolFeatures(negotiated)})
			continue
		}
		if granted.storeID != "" && !appIDMatches(granted.storeID, request.AppID) {
			sendFrameError(socket, ctx, request, "Frame app id does not match the paired app")
			continue
		}
		auth := granted
		request.auth = &auth
		switch request.Type {
		case "ping":
			send(socket, ctx, map[string]any{"type": "pong", "id": request.ID, "now": time.Now().UnixMilli()})
		case "fs":
			if !socketAllows(granted.capabilities, filesystemCapability(request.Method)) {
				sendError(socket, ctx, request.ID, permissionDeniedError(filesystemCapability(request.Method)))
				continue
			}
			s.handleFilesystem(socket, ctx, request, body)
		case "exec":
			required := "fs.execFile"
			if request.Command != "" {
				required = "fs.exec"
			}
			if !socketAllows(granted.capabilities, required) {
				send(socket, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": permissionDeniedError(required)})
				continue
			}
			s.handleExec(socket, ctx, processes, request)
		case "process":
			processes.handle(request)
		case "rpc":
			if required := rpcCapability(request); !socketAllows(granted.capabilities, required) {
				rpcError(socket, ctx, request, errors.New(permissionDeniedError(required)))
				continue
			}
			s.handleRPC(socket, ctx, agents, request, body)
		case "net":
			if !socketAllows(granted.capabilities, "net."+request.ProtocolName+".connect") && !socketAllows(granted.capabilities, "net.protocol."+request.ProtocolName) {
				sendError(socket, ctx, request.ID, permissionDeniedError(networkCapability(request.ProtocolName)))
				continue
			}
			s.handleNetwork(socket, ctx, bridges, request, body)
		case "net-data":
			s.handleNetwork(socket, ctx, bridges, message{Action: "data", BridgeID: request.BridgeID}, body)
		case "net-close":
			s.handleNetwork(socket, ctx, bridges, message{Action: "close", BridgeID: request.BridgeID}, nil)
		default:
			send(socket, ctx, map[string]any{"type": "error", "id": request.ID, "error": "Unknown remote request"})
		}
	}
}

// relayGrant computes a relay socket's capabilities: the synced browser
// identity's grant intersected with the manifest of the app the hello names.
// Without an app block (or when Dyner cannot resolve it) the identity grant
// alone applies. A hello that names no known identity is limited to what
// every synced identity holds, so it never exceeds any authority decision.
func (s *Server) relayGrant(ctx context.Context, config Config, hello message) socketAuthentication {
	s.mu.Lock()
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	return relayGrantFor(identities, hello, func(storeID string) (resolvedApp, bool) {
		resolved, err := s.resolveManifestApp(ctx, config, storeID)
		return resolved, err == nil
	})
}

func relayGrantFor(identities []BrowserIdentity, hello message, resolve func(storeID string) (resolvedApp, bool)) socketAuthentication {
	var base []string
	keyID := strings.TrimSpace(hello.KeyID)
	matched := false
	connect := map[string][]string{}
	if keyID != "" {
		for _, identity := range identities {
			if identity.KeyID != keyID || identity.Authority {
				continue
			}
			if identity.StoreID != "" && hello.App != nil && identity.StoreID != strings.TrimSpace(hello.App.StoreID) {
				continue
			}
			base = unionStrings(base, identity.Capabilities)
			for scheme, patterns := range identity.Connect {
				connect[scheme] = unionStrings(connect[scheme], patterns)
			}
			matched = true
		}
	}
	if !matched {
		// Without a bound identity the HTTP handler keeps honoring the
		// request's own connect list, as before.
		keyID = ""
	}
	if !matched {
		for index, identity := range identities {
			if identity.Authority {
				continue
			}
			if index == 0 || base == nil {
				base = normalizePermissionSet(identity.Capabilities)
				continue
			}
			base = intersectStrings(base, identity.Capabilities)
		}
	}
	auth := socketAuthentication{capabilities: normalizePermissionSet(base), keyID: keyID, ok: true}
	auth.declared = auth.capabilities
	if matched {
		auth.connect = connect
	}
	if hello.App != nil {
		if storeID := strings.TrimSpace(hello.App.StoreID); validStoreID(storeID) {
			auth.storeID = storeID
			auth.appName = storeSlug(storeID)
			if resolved, ok := resolve(storeID); ok {
				auth.capabilities = normalizePermissionSet(intersectStrings(auth.capabilities, resolved.Declared))
				auth.declared = resolved.Declared
				auth.connect = resolved.Connect
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
