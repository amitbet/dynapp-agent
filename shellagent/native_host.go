package shellagent

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// The native channel carries the unchanged agent protocol between an
// installed app's host process and the agent (docs/native-host.md).
const (
	nativeFrameText       byte = 1
	nativeFrameBinary     byte = 2
	maxNativeFrame             = maxReadBytes + 1024*1024
	nativeHelloTimeout         = 10 * time.Second
	nativeReviewTimeout        = 10 * time.Minute
	nativeProtocolVersion      = 1
)

//go:embed native_host_bridge.js
var nativeBridgeTemplate string

// verifyNativePeer is replaceable so tests can drive the channel over pipes.
var verifyNativePeer = nativeVerifyPeer

// verifyNativePlatformPeer authenticates a platform host connection.
var verifyNativePlatformPeer = nativeVerifyPlatformPeer

// nativeFrameSocket implements protocolSocket over one stream connection.
type nativeFrameSocket struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func newNativeFrameSocket(conn net.Conn) *nativeFrameSocket {
	return &nativeFrameSocket{conn: conn, reader: bufio.NewReaderSize(conn, 64*1024)}
}

func (socket *nativeFrameSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	var header [5]byte
	if _, err := io.ReadFull(socket.reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length < 1 || length > maxNativeFrame {
		return 0, nil, errors.New("native frame length is invalid")
	}
	payload := make([]byte, length-1)
	if _, err := io.ReadFull(socket.reader, payload); err != nil {
		return 0, nil, err
	}
	switch header[4] {
	case nativeFrameText:
		return websocket.MessageText, payload, nil
	case nativeFrameBinary:
		return websocket.MessageBinary, payload, nil
	default:
		return 0, nil, errors.New("native frame kind is invalid")
	}
}

func (socket *nativeFrameSocket) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data)+1 > maxNativeFrame {
		return errors.New("native frame is too large")
	}
	frameKind := nativeFrameText
	if kind == websocket.MessageBinary {
		frameKind = nativeFrameBinary
	}
	frame := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)+1))
	frame[4] = frameKind
	copy(frame[5:], data)
	socket.writeMu.Lock()
	defer socket.writeMu.Unlock()
	_, err := socket.conn.Write(frame)
	return err
}

// Close tells the host how the connection ended so the page sees the same
// close code a WebSocket would report, then closes the stream.
func (socket *nativeFrameSocket) Close(code websocket.StatusCode, reason string) error {
	socket.closeOnce.Do(func() {
		// A host that has stopped reading must not stall the caller.
		_ = socket.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = socket.Write(context.Background(), websocket.MessageText, mustJSON(map[string]any{
			"type": "native-host-close", "code": int(code), "reason": reason,
		}))
		_ = socket.conn.Close()
	})
	return nil
}

type nativeHello struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	StoreID string `json:"storeId"`
	Purpose string `json:"purpose"`
}

type nativeHostFrame struct {
	Paths        []string `json:"paths,omitempty"`
	Type         string   `json:"type"`
	ID           string   `json:"id,omitempty"`
	RequestID    string   `json:"requestId,omitempty"`
	Capabilities []string `json:"capabilities"`
}

// nativeAsk is one permission decision shared by every connection of an app
// that is waiting for it.
type nativeAsk struct {
	pending  *pendingRequest
	kind     string
	ask      []string
	frame    []byte
	done     chan struct{}
	granted  []string
	approved bool
	decided  bool
}

func (s *Server) nativeEndpoint() string {
	return nativeEndpoint(s.StateDir)
}

// startNativeHost opens the private endpoint installed apps connect to. It
// never prevents the agent from starting.
func (s *Server) startNativeHost() {
	if supported, _ := s.nativeAvailable(); !supported {
		return
	}
	endpoint := s.nativeEndpoint()
	if endpoint == "" {
		return
	}
	listener, err := nativeListen(endpoint)
	if err != nil {
		log.Printf("DynApp Shell agent: native app endpoint unavailable: %v", err)
		return
	}
	s.mu.Lock()
	s.nativeListener = listener
	s.mu.Unlock()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("DynApp Shell agent: native app accept failed: %v", err)
				time.Sleep(200 * time.Millisecond)
				continue
			}
			go s.serveNativeConnection(conn)
		}
	}()
}

func (s *Server) stopNativeHost() {
	s.mu.Lock()
	listener := s.nativeListener
	s.nativeListener = nil
	conns := s.nativeConns
	s.nativeConns = nil
	s.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	// Running app hosts see the same drop as an agent restart and reconnect.
	for conn := range conns {
		_ = conn.Close()
	}
}

func (s *Server) serveNativeConnection(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	s.mu.Lock()
	if s.nativeConns == nil {
		s.nativeConns = map[net.Conn]struct{}{}
	}
	s.nativeConns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.nativeConns, conn)
		s.mu.Unlock()
	}()
	socket := newNativeFrameSocket(conn)
	_ = conn.SetReadDeadline(time.Now().Add(nativeHelloTimeout))
	kind, data, err := socket.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		return
	}
	var hello nativeHello
	if json.Unmarshal(data, &hello) != nil || hello.Type != "native-host-hello" || hello.Version != nativeProtocolVersion {
		nativeReject(socket, "The native app handshake is invalid")
		return
	}
	if hello.Purpose == "platform" {
		if !nativePlatformChannelEnabled() {
			nativeReject(socket, "This agent has no platform host channel")
			return
		}
		if err := verifyNativePlatformPeer(conn); err != nil {
			log.Printf("DynApp Shell agent: rejected native platform host: %v", err)
			nativeReject(socket, "This process may not act as the DynApp platform host")
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		s.serveNativePlatform(ctx, socket)
		return
	}
	app, ok := s.nativeApp(strings.TrimSpace(hello.StoreID))
	if !ok {
		nativeReject(socket, "This app is not installed as a native app. Install it again from Dyner.")
		return
	}
	if err := verifyNativePeer(conn, app); err != nil {
		log.Printf("DynApp Shell agent: rejected native host for %s: %v", app.StoreID, err)
		nativeReject(socket, "This app copy is not the one DynApp installed. Install it again from Dyner.")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch hello.Purpose {
	case "control":
		s.serveNativeControl(ctx, socket, app)
	case "app", "":
		auth, err := s.authorizeNativeApp(ctx, socket, app)
		if err != nil {
			return
		}
		current, _ := s.nativeApp(app.StoreID)
		if !send(socket, ctx, nativeReady(current, auth.capabilities, "")) {
			return
		}
		s.serveAuthenticatedSocket(ctx, socket, func(message) socketAuthentication { return auth }, carrierCapabilities{}, nil)
	default:
		nativeReject(socket, "The native app handshake is invalid")
	}
}

func nativeReject(socket *nativeFrameSocket, text string) {
	_ = socket.Write(context.Background(), websocket.MessageText, mustJSON(map[string]any{"type": "native-host-error", "error": text}))
	_ = socket.conn.Close()
}

func nativeReady(app NativeApp, capabilities []string, bootstrap string) map[string]any {
	if capabilities == nil {
		capabilities = []string{}
	}
	ready := map[string]any{
		"type": "native-host-ready", "version": nativeProtocolVersion,
		"storeId": app.StoreID, "appName": app.Name, "origin": app.Origin, "url": app.URL,
		"capabilities": capabilities,
	}
	if orientation := validOrientation(app.Orientation); orientation != "" {
		ready["orientation"] = orientation
	}
	if bootstrap != "" {
		ready["bootstrapScript"] = bootstrap
	}
	return ready
}

// nativeBootstrapScript is the page bridge the host injects before any page
// script runs.
func nativeBootstrapScript(app NativeApp) string {
	config := mustJSON(map[string]any{
		"version": nativeProtocolVersion, "platform": nativePlatformName(),
		"storeId": app.StoreID, "origin": app.Origin,
	})
	return strings.Replace(nativeBridgeTemplate, "__DYNAPP_NATIVE_HOST_CONFIG__", string(config), 1)
}

// authorizeNativeApp computes the connection's capabilities. Permissions the
// user has not reviewed are asked in the app's own window first.
func (s *Server) authorizeNativeApp(ctx context.Context, socket *nativeFrameSocket, app NativeApp) (socketAuthentication, error) {
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	manifest, err := s.resolveNativeManifest(resolveCtx, config, app.StoreID)
	cancel()
	if err != nil {
		// Offline: keep working with the stored grant and no new permissions.
		return authForNative(config, app, nil), nil
	}
	if app.Authority {
		return authForNative(config, app, &manifest.app), nil
	}
	declared := normalizePermissionSet(subtractStrings(manifest.app.Declared, []string{permissionsManage}))
	kind, ask := "", []string(nil)
	if app.ApprovedAt == 0 {
		kind, ask = "pairing", declared
	} else if fresh := subtractStrings(declared, app.Declared); len(fresh) > 0 {
		kind, ask = "delta", normalizePermissionSet(fresh)
	}
	if kind != "" && len(ask) > 0 {
		decision, err := s.awaitNativeDecision(ctx, socket, app, manifest, kind, ask)
		if err != nil {
			return socketAuthentication{}, err
		}
		if decision.decided {
			updated, _ := s.nativeApp(app.StoreID)
			granted := intersectStrings(decision.granted, ask)
			if kind == "pairing" {
				updated.Capabilities = normalizePermissionSet(granted)
			} else {
				updated.Capabilities = normalizePermissionSet(unionStrings(updated.Capabilities, granted))
			}
			updated.Declared = normalizePermissionSet(unionStrings(updated.Declared, declared))
			if updated.ApprovedAt == 0 {
				updated.ApprovedAt = time.Now().UnixMilli()
			}
			updated.Connect = manifest.app.Connect
			s.upsertNativeApp(updated)
			go s.notifyPermissionsChanged()
			app = updated
		}
	} else if kind == "pairing" {
		// Nothing declared: record the (empty) decision so it is not asked.
		app.ApprovedAt = time.Now().UnixMilli()
		app.Declared = declared
		s.upsertNativeApp(app)
	}
	return authForNative(config, app, &manifest.app), nil
}

// awaitNativeDecision sends the permission request to this host and waits for
// the user's answer from any connection of the app, or from Dyner.
func (s *Server) awaitNativeDecision(ctx context.Context, socket *nativeFrameSocket, app NativeApp, manifest nativeManifest, kind string, ask []string) (*nativeAsk, error) {
	shared, err := s.sharedNativeAsk(app, manifest, kind, ask)
	if err != nil {
		return nil, err
	}
	if err := socket.Write(ctx, websocket.MessageText, shared.frame); err != nil {
		return nil, err
	}
	frames := make(chan []byte, 4)
	readErr := make(chan error, 1)
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	go func() {
		for {
			messageType, data, err := socket.Read(readCtx)
			if err != nil {
				readErr <- err
				return
			}
			if messageType != websocket.MessageText {
				continue
			}
			select {
			case frames <- data:
			case <-readCtx.Done():
				return
			}
			if readCtx.Err() != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-shared.done:
			// The reader goroutine may still be blocked in Read. Release it by
			// handing the connection back only after it returns.
			stopReading()
			return shared, s.drainNativeReader(socket, frames, readErr)
		case data := <-frames:
			var frame nativeHostFrame
			if json.Unmarshal(data, &frame) == nil && frame.Type == "native-host-permission-decision" && frame.RequestID == shared.pending.ID {
				approved := frame.Capabilities != nil
				shared.pending.resolve(intersectStrings(frame.Capabilities, shared.pending.Declared), approved)
			}
		case err := <-readErr:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// drainNativeReader waits for the temporary reader to stop. The host sends
// nothing between its decision and native-host-ready, so a pending Read is
// unblocked with a short deadline.
func (s *Server) drainNativeReader(socket *nativeFrameSocket, frames chan []byte, readErr chan error) error {
	_ = socket.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	defer socket.conn.SetReadDeadline(time.Time{})
	select {
	case err := <-readErr:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return err
	case <-frames:
		return errors.New("native host sent a frame before the app was ready")
	case <-time.After(2 * time.Second):
		return errors.New("native host reader did not stop")
	}
}

// sharedNativeAsk returns the in-flight decision for an app or registers a
// new one. Dyner's Permissions screen can decide it too.
func (s *Server) sharedNativeAsk(app NativeApp, manifest nativeManifest, kind string, ask []string) (*nativeAsk, error) {
	s.nativeAskMu.Lock()
	defer s.nativeAskMu.Unlock()
	if s.nativeAsks == nil {
		s.nativeAsks = map[string]*nativeAsk{}
	}
	if existing := s.nativeAsks[app.StoreID]; existing != nil {
		select {
		case <-existing.done:
		default:
			return existing, nil
		}
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: kind, AppName: manifest.app.Name, StoreID: app.StoreID, Origin: app.Origin, Declared: ask,
	})
	if err != nil {
		return nil, err
	}
	shared := &nativeAsk{pending: pending, kind: kind, ask: ask, done: make(chan struct{})}
	shared.frame = mustJSON(map[string]any{
		"type": "native-host-permission-request", "requestId": pending.ID, "kind": kind,
		"appName": manifest.app.Name, "origin": app.Origin,
		"groups": nativePermissionGroups(ask, app.Capabilities, manifest.reasons),
	})
	s.nativeAsks[app.StoreID] = shared
	go func() {
		granted, approved := s.awaitDecision(context.Background(), pending)
		// awaitDecision reports an expired request as declined. Only a
		// resolved request is a decision worth recording.
		shared.granted, shared.approved = granted, approved
		shared.decided = pending.wasResolved()
		close(shared.done)
		s.nativeAskMu.Lock()
		if s.nativeAsks[app.StoreID] == shared {
			delete(s.nativeAsks, app.StoreID)
		}
		s.nativeAskMu.Unlock()
	}()
	return shared, nil
}

// serveNativeControl answers the host's launch-time bootstrap request and its
// "Permissions…" reviews for one app.
func (s *Server) serveNativeControl(ctx context.Context, socket *nativeFrameSocket, app NativeApp) {
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	if !send(socket, ctx, nativeReady(app, authForNative(config, app, nil).capabilities, nativeBootstrapScript(app))) {
		return
	}
	// The ready frame carries the stored orientation; check Dyner for a newer
	// one without holding up the launch.
	go func() {
		refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if orientation, changed := s.refreshNativeOrientation(refreshCtx, app.StoreID); changed {
			send(socket, ctx, map[string]any{"type": "native-host-display", "orientation": orientation})
		}
	}()
	for {
		messageType, data, err := socket.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame nativeHostFrame
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		if frame.Type == "native-host-open-files" {
			err := s.queueNativeOpenFiles(app.StoreID, frame.Paths)
			result := map[string]any{"type": "native-host-open-files-result", "id": frame.ID}
			if err != nil {
				result["error"] = err.Error()
			}
			if !send(socket, ctx, result) {
				return
			}
			continue
		}
		if frame.Type != "native-host-review" {
			continue
		}
		result, updated, err := s.reviewNativeApp(ctx, socket, app.StoreID)
		if err != nil {
			send(socket, ctx, map[string]any{"type": "native-host-review-result", "id": frame.ID, "error": err.Error()})
			continue
		}
		result["type"] = "native-host-review-result"
		result["id"] = frame.ID
		ok := send(socket, ctx, result)
		// Reply first: closing the page sockets makes the page reconnect with
		// the new grant, and the host reloads it on "changed".
		if updated != nil {
			s.closeNativeSockets(*updated, websocket.StatusServiceRestart, "Permissions changed")
			go s.notifyPermissionsChanged()
		}
		if !ok {
			return
		}
	}
}

// reviewNativeApp asks about every declared permission and stores the answer.
func (s *Server) reviewNativeApp(ctx context.Context, socket *nativeFrameSocket, storeID string) (map[string]any, *NativeApp, error) {
	app, ok := s.nativeApp(storeID)
	if !ok {
		return nil, nil, errors.New("this app is no longer installed")
	}
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	manifest, err := s.resolveNativeManifest(resolveCtx, config, storeID)
	cancel()
	declared := normalizePermissionSet(app.Declared)
	reasons := map[string]string{}
	name := app.Name
	if err == nil {
		declared = normalizePermissionSet(unionStrings(declared, manifest.app.Declared))
		reasons = manifest.reasons
		name = manifest.app.Name
	}
	declared = subtractStrings(declared, []string{permissionsManage})
	if len(declared) == 0 {
		return map[string]any{"changed": false, "capabilities": normalizePermissionSet(app.Capabilities)}, nil, nil
	}
	requestID, err := requestID()
	if err != nil {
		return nil, nil, err
	}
	if err := socket.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
		"type": "native-host-permission-request", "requestId": requestID, "kind": "review",
		"appName": name, "origin": app.Origin,
		"groups": nativePermissionGroups(declared, app.Capabilities, reasons),
	})); err != nil {
		return nil, nil, err
	}
	deadline, cancelDeadline := context.WithTimeout(ctx, nativeReviewTimeout)
	defer cancelDeadline()
	for {
		messageType, data, err := socket.Read(deadline)
		if err != nil {
			return nil, nil, err
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame nativeHostFrame
		if json.Unmarshal(data, &frame) != nil || frame.Type != "native-host-permission-decision" || frame.RequestID != requestID {
			continue
		}
		if frame.Capabilities == nil {
			return map[string]any{"changed": false, "capabilities": normalizePermissionSet(app.Capabilities)}, nil, nil
		}
		granted := normalizePermissionSet(intersectStrings(frame.Capabilities, declared))
		if app.Authority {
			granted = unionStrings(granted, []string{permissionsManage})
		}
		changed := !sameCapabilitySet(granted, app.Capabilities)
		app.Capabilities = granted
		app.Declared = normalizePermissionSet(unionStrings(app.Declared, declared))
		if app.ApprovedAt == 0 {
			app.ApprovedAt = time.Now().UnixMilli()
		}
		s.upsertNativeApp(app)
		result := map[string]any{"changed": changed, "capabilities": granted}
		if changed {
			return result, &app, nil
		}
		return result, nil, nil
	}
}

func (app NativeApp) String() string { return fmt.Sprintf("%s (%s)", app.Name, app.StoreID) }

// Only the authenticated native host can submit paths for its own app.
// The page can consume them only with the externalOpen.files grant.
func (s *Server) queueNativeOpenFiles(storeID string, paths []string) error {
	app, ok := s.nativeApp(storeID)
	if !ok {
		return errors.New("native app is no longer installed")
	}
	s.mu.Lock()
	capabilities := authForNative(s.Config, app, nil).capabilities
	s.mu.Unlock()
	allowed := false
	for _, capability := range capabilities {
		if capability == "externalOpen.files" {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("Allow opening external files in this app's permissions first")
	}
	if len(paths) == 0 || len(paths) > 64 {
		return errors.New("invalid number of files")
	}
	valid := make([]string, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return errors.New("file path must be absolute")
		}
		valid = append(valid, filepath.Clean(path))
	}
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	if s.externalOpens == nil {
		s.externalOpens = map[string][]string{}
	}
	s.externalOpens[storeID] = append(s.externalOpens[storeID], valid...)
	return nil
}
