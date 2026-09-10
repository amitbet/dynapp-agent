// Package shellagent implements the headless DynApp Shell agent.
//
// Its loopback WebSocket protocol intentionally matches the existing
// RemoteEnvironmentServer protocol. That lets the PWA Shell keep its existing
// appShell injectables while Electron is migrated capability by capability.
package shellagent

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

const (
	// ProtocolVersion is the current remote-environment wire protocol. The
	// agent still accepts v1 clients during the rollout.
	ProtocolVersion       = 2
	LegacyProtocolVersion = 1
	DefaultAddress        = "127.0.0.1:9011"
	RemotePath            = "/dynapp-remote"
	maxReadBytes          = 20 * 1024 * 1024
	maxWriteBytes         = 20 * 1024 * 1024
	maxChunkBytes         = 320 * 1024
	maxExecArgs           = 128
	maxExecArgumentBytes  = 4096
	maxExecEnvVars        = 64
	maxExecEnvBytes       = 16 * 1024
	maxExecOutputBytes    = 8 * 1024 * 1024
	maxExecDuration       = 60 * time.Second
)

// Server is the local, unprivileged-by-default environment provider. It is
// deliberately usable in-process in tests and as a background OS service.
type Server struct {
	Address  string
	StateDir string
	// SelfUpdate controls the optional GitHub release poller. The command
	// package supplies the callback that restarts the process after a verified
	// binary has been staged.
	SelfUpdate SelfUpdateConfig
	// Config is optional for a local-only agent. When enrolled with relay
	// enabled it starts the account-configured hosted connection as well.
	Config Config
	// AccountToken is the signed-in Dyner user bearer token used to create
	// device credentials the same way Electron does. It is never persisted in
	// the agent config and never exposed over the privileged protocol.
	AccountToken string
	// DynerHTTPClient is used for unauthenticated app lookups during pairing.
	DynerHTTPClient *http.Client

	mu                 sync.Mutex
	http               *http.Server
	relayCancel        context.CancelFunc
	identitySyncCancel context.CancelFunc
	selfUpdateCancel   context.CancelFunc
	lanClose           func() error
	bridgeCounter      atomic.Uint64
	activeBridges      atomic.Int64
	updateMu           sync.RWMutex
	updatePending      bool
	externalMu         sync.Mutex
	externalOpens      map[string][]string
	previewMu          sync.Mutex
	previews           map[string]*livePreview
	promiseMu          sync.Mutex
	promises           map[string]*clipboardPromise
	promiseBridge      *filePromiseBridge
	testWebSocket      bool
	// autoApprovePairings lets in-process tests skip the authority decision;
	// production never sets it.
	autoApprovePairings  bool
	permissionState      *permissionsState
	csrfMu               sync.Mutex
	csrfToken            string
	presentationMu       sync.Mutex
	presentations        map[*presentationBridge]struct{}
	presentationClosed   bool
	chromiumRepairCancel context.CancelFunc
}

type protocolSocket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
}

var connectionWriters sync.Map // map[protocolSocket]*sync.Mutex
var connectionE2EE sync.Map    // map[protocolSocket]relayCipher

type relayCipher struct{ tokenHash string }
type socketAuthentication struct {
	environment map[string]any
	// capabilities is the effective set: the authority's grant intersected
	// with the app manifest's declared permissions.
	capabilities []string
	// stored mirrors the persisted identity capabilities for revocation checks.
	stored []string
	// declared is the app's full manifest permission set.
	declared []string
	// connect is the approved HTTP connect allowlist per scheme.
	connect     map[string][]string
	keyID       string
	origin      string
	storeID     string
	appName     string
	development bool
	ok          bool
}

func (auth socketAuthentication) allows(required string) bool {
	return socketAllows(auth.capabilities, required)
}

// Handler exposes the stable local remote-environment endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.testWebSocket {
		mux.HandleFunc(RemotePath, s.handleTestWebSocket)
	} else {
		mux.HandleFunc(RemotePath, s.handleLocalWebSocket)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "protocol": ProtocolVersion})
	})
	mux.HandleFunc("/.well-known/dynapp-direct-local", s.handleDirectLocalInfo)
	mux.HandleFunc("/external-open", s.handleExternalOpenHTTP)
	s.registerSettingsHandlers(mux)
	return mux
}

// testDevelopmentOrigin stands in for a browser Origin when the in-process
// test carrier dials without one. It is a loopback development origin, so it
// follows exactly the same handshake as a real browser.
const testDevelopmentOrigin = "http://localhost"

func (s *Server) handleLocalWebSocket(w http.ResponseWriter, r *http.Request) {
	s.serveLocalWebSocket(w, r, false)
}

func (s *Server) handleTestWebSocket(w http.ResponseWriter, r *http.Request) {
	s.serveLocalWebSocket(w, r, true)
}

// serveLocalWebSocket applies the transport checks (loopback Host, Origin
// allowlist), runs the direct-local challenge, and only then serves frames.
func (s *Server) serveLocalWebSocket(w http.ResponseWriter, r *http.Request, test bool) {
	if !loopbackHostHeader(r.Host) {
		http.Error(w, "the Shell agent only accepts loopback hosts", http.StatusForbidden)
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" && test {
		origin = testDevelopmentOrigin
	}
	parsedOrigin, ok := parseOrigin(origin)
	if !ok || !s.originAllowed(origin) {
		http.Error(w, "origin is not allowed to connect to the Shell agent", http.StatusForbidden)
		return
	}
	origin = strings.ToLower(parsedOrigin.Scheme + "://" + parsedOrigin.Host)
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{parsedOrigin.Host}})
	if err != nil {
		return
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	connection.SetReadLimit(maxReadBytes + 512*1024)
	ctx := r.Context()
	auth, outcome := s.authenticateDirectLocal(ctx, connection, origin)
	switch outcome {
	case directLocalOK:
	case directLocalProbe, directLocalAborted:
		return
	case directLocalApprovalRequired:
		_ = connection.Close(closeApprovalRequired, "Open this app from Dyner to approve access")
		return
	case directLocalDeclined:
		_ = connection.Close(closePairingDeclined, "Pairing declined")
		return
	default:
		_ = connection.Close(closeIdentityRejected, "Browser identity rejected")
		return
	}
	s.serveAuthenticatedSocket(ctx, connection, func(message) socketAuthentication { return auth }, false, nil)
}

// ListenAndServe starts the agent on its configured loopback address.
func (s *Server) ListenAndServe() error {
	address := s.Address
	if address == "" {
		address = DefaultAddress
	}
	s.mu.Lock()
	if s.http != nil {
		s.mu.Unlock()
		return errors.New("shell agent is already running")
	}
	s.http = &http.Server{Addr: address, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	httpServer := s.http
	config := s.Config
	s.mu.Unlock()
	if config.LANEnabled && config.ListenerMode != ListenerOff {
		closeLAN, err := s.startLAN(config)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.lanClose = closeLAN
		s.mu.Unlock()
	}
	if err := s.ensureListenerRegistration(context.Background()); err != nil {
		log.Printf("DynApp Shell agent: could not register this device with Dyner: %v", err)
	}
	s.startIdentitySync()
	s.startRelay()
	s.startSelfUpdater()
	s.startChromiumDesktopRepair()
	hydrateExecutablePath()
	if managedNodeAvailable(s.StateDir) {
		prependPathDir(managedNodeBinDir(s.StateDir))
	}
	return httpServer.ListenAndServe()
}

func (s *Server) startRelay() {
	s.mu.Lock()
	if s.relayCancel != nil {
		s.mu.Unlock()
		return
	}
	config := s.Config.relayTicketConfig()
	if !config.RelayEnabled || config.DeviceCredential == "" {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.relayCancel = cancel
	s.mu.Unlock()
	go s.runRelay(ctx, config)
}

func (s *Server) stopRelay() {
	s.mu.Lock()
	cancel := s.relayCancel
	s.relayCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) startIdentitySync() {
	s.mu.Lock()
	if s.identitySyncCancel != nil || s.Config.DeviceCredential == "" {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.identitySyncCancel = cancel
	s.mu.Unlock()
	s.syncBrowserIdentitiesOnce(ctx)
	go s.syncBrowserIdentities(ctx)
}

func (s *Server) restartLAN() error {
	s.mu.Lock()
	closeLAN := s.lanClose
	s.lanClose = nil
	config := s.Config
	s.mu.Unlock()
	if closeLAN != nil {
		_ = closeLAN()
	}
	if !config.LANEnabled || config.ListenerMode == ListenerOff {
		return nil
	}
	started, err := s.startLAN(config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lanClose = started
	s.mu.Unlock()
	return nil
}

func (s *Server) syncBrowserIdentities(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
			s.syncBrowserIdentitiesOnce(ctx)
		}
	}
}

func (s *Server) syncBrowserIdentitiesOnce(ctx context.Context) {
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	identities, err := SyncBrowserIdentities(ctx, nil, config)
	if err != nil {
		var httpError dynerHTTPError
		if !errors.As(err, &httpError) || (httpError.StatusCode != http.StatusUnauthorized && httpError.StatusCode != http.StatusNotFound && httpError.StatusCode != http.StatusGone) {
			return
		}
		identities = nil
	}
	s.mu.Lock()
	s.Config.BrowserIdentities = mergeSyncedIdentities(s.Config.BrowserIdentities, identities)
	snapshot := s.Config
	s.mu.Unlock()
	_ = SaveConfig(s.StateDir, snapshot)
}

// Shutdown stops a running service host cleanly.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	httpServer := s.http
	relayCancel := s.relayCancel
	identitySyncCancel := s.identitySyncCancel
	selfUpdateCancel := s.selfUpdateCancel
	chromiumRepairCancel := s.chromiumRepairCancel
	lanClose := s.lanClose
	s.mu.Unlock()
	if relayCancel != nil {
		relayCancel()
	}
	if identitySyncCancel != nil {
		identitySyncCancel()
	}
	if selfUpdateCancel != nil {
		selfUpdateCancel()
	}
	if chromiumRepairCancel != nil {
		chromiumRepairCancel()
	}
	if lanClose != nil {
		_ = lanClose()
	}
	s.closeLivePreviews()
	s.closePresentations()
	if httpServer == nil {
		return nil
	}
	return httpServer.Shutdown(ctx)
}

// ActiveBridges reports the number of TCP, UDP, and RDP bridges currently
// owned by the agent. An update must not replace the process while this is
// non-zero because the connections live inside the process.
func (s *Server) ActiveBridges() int {
	count := s.activeBridges.Load()
	if count < 0 {
		return 0
	}
	return int(count)
}

func (s *Server) lockBridgeOpen() (func(), bool) {
	s.updateMu.RLock()
	if s.updatePending {
		s.updateMu.RUnlock()
		return nil, false
	}
	return s.updateMu.RUnlock, true
}

func (s *Server) beginUpdate() bool {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.updatePending || s.activeBridges.Load() != 0 {
		return false
	}
	s.updatePending = true
	return true
}

func (s *Server) cancelUpdate() {
	s.updateMu.Lock()
	s.updatePending = false
	s.updateMu.Unlock()
}

type message struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Protocol  json.RawMessage `json:"protocol,omitempty"`
	Local     bool            `json:"local,omitempty"`
	TokenHash string          `json:"tokenHash,omitempty"`
	Token     string          `json:"token,omitempty"`
	// KeyID and App let a relay hello name the browser identity and app so
	// the agent can intersect the synced grant with the manifest.
	KeyID        string          `json:"keyId,omitempty"`
	App          *directLocalApp `json:"app,omitempty"`
	Method       string          `json:"method,omitempty"`
	Service      string          `json:"service,omitempty"`
	AppID        string          `json:"appId,omitempty"`
	SessionID    string          `json:"sessionId,omitempty"`
	Args         []any           `json:"args,omitempty"`
	File         string          `json:"file,omitempty"`
	Command      string          `json:"command,omitempty"`
	Cwd          string          `json:"cwd,omitempty"`
	Action       string          `json:"action,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
	Stdin        string          `json:"stdin,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	Cols         int             `json:"cols,omitempty"`
	Rows         int             `json:"rows,omitempty"`
	Term         string          `json:"term,omitempty"`
	Env          map[string]any  `json:"env,omitempty"`
	ProcessID    string          `json:"processId,omitempty"`
	Data         string          `json:"data,omitempty"`
	Signal       string          `json:"signal,omitempty"`
	BridgeID     string          `json:"bridgeId,omitempty"`
	ProtocolName string          `json:"-"`
	Target       struct {
		Host      string `json:"host"`
		Port      int    `json:"port"`
		LocalPort int    `json:"localPort,omitempty"`
	} `json:"target,omitempty"`
	// auth is the authenticated socket the frame arrived on. Handlers use it
	// for capability crossovers and the HTTP connect allowlist.
	auth *socketAuthentication
}

// allows reports whether the frame's socket holds a capability. Frames built
// outside a socket (direct handler calls) have no socket and are trusted.
func (m message) allows(required string) bool {
	if m.auth == nil {
		return true
	}
	return m.auth.allows(required)
}

func (s *Server) serveAuthenticatedSocket(ctx context.Context, connection protocolSocket, authenticate func(message) socketAuthentication, reliableStreams bool, channels <-chan protocolSocket) {
	connectionWriters.Store(connection, &sync.Mutex{})
	defer connectionWriters.Delete(connection)
	bridges := newBridgeSet(&s.activeBridges)
	defer bridges.closeAll()
	if channels != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case channel, ok := <-channels:
					if !ok {
						return
					}
					go s.handleReliableChannel(ctx, channel, bridges)
				}
			}
		}()
	}
	processes := newProcessSet(connection, ctx)
	defer processes.closeAll()
	var presentation *presentationBridge
	defer func() {
		if presentation != nil {
			presentation.close()
		}
	}()
	agents := newAgentService(s, connection)
	defer agents.closeAll()
	_, data, err := connection.Read(ctx)
	if err != nil {
		return
	}
	var hello message
	if err := json.Unmarshal(data, &hello); err != nil {
		_ = connection.Close(websocket.StatusPolicyViolation, "Authentication required")
		return
	}
	negotiatedProtocol := protocolNumber(hello.Protocol)
	authentication := authenticate(hello)
	if hello.Type != "hello" || (negotiatedProtocol != LegacyProtocolVersion && negotiatedProtocol != ProtocolVersion) || !authentication.ok {
		_ = connection.Close(websocket.StatusPolicyViolation, "Authentication required")
		return
	}
	if authentication.keyID != "" {
		defer s.trackLiveSocket(connection, &authentication)()
	}
	if !send(connection, ctx, map[string]any{
		"type": "hello", "ok": true, "protocol": negotiatedProtocol,
		"serverId":     "go-shell-agent",
		"environment":  authentication.environment,
		"capabilities": authentication.capabilities,
		"features":     protocolFeatures(negotiatedProtocol, reliableStreams),
	}) {
		return
	}
	for {
		messageType, data, err := connection.Read(ctx)
		if err != nil {
			return
		}
		var request message
		var body []byte
		if messageType == websocket.MessageBinary {
			request, body, err = decodeBinaryFrame(data)
		} else {
			err = json.Unmarshal(data, &request)
		}
		if err != nil {
			sendError(connection, ctx, "", "Remote message is invalid")
			continue
		}
		request.ProtocolName = protocolString(request.Protocol)
		if authentication.keyID != "" && !s.hasBrowserIdentity(authentication) {
			_ = connection.Close(closePairingDeclined, "Pairing revoked")
			return
		}
		// Per-socket app binding: frames may only name the app the socket was
		// paired for, and every keyed service uses the bound id.
		if authentication.storeID != "" {
			if !appIDMatches(authentication.storeID, request.AppID) {
				sendFrameError(connection, ctx, request, "Frame app id does not match the paired app")
				continue
			}
			request.AppID = authentication.storeID
		}
		request.auth = &authentication
		switch request.Type {
		case "ping":
			send(connection, ctx, map[string]any{"type": "pong", "id": request.ID, "now": time.Now().UnixMilli()})
		case "fs":
			required := filesystemCapability(request.Method)
			if !socketAllows(authentication.capabilities, required) {
				sendError(connection, ctx, request.ID, permissionDeniedError(required))
				continue
			}
			s.handleFilesystem(connection, ctx, request, body)
		case "exec":
			required := "fs.execFile"
			if request.Command != "" {
				required = "fs.exec"
			}
			if !socketAllows(authentication.capabilities, required) {
				send(connection, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": permissionDeniedError(required)})
				continue
			}
			s.handleExec(connection, ctx, processes, request)
		case "process":
			processes.handle(request)
		case "rpc":
			if required := rpcCapability(request); !socketAllows(authentication.capabilities, required) {
				rpcError(connection, ctx, request, errors.New(permissionDeniedError(required)))
				continue
			}
			if request.Service == "tray" || request.Service == "globalShortcut" || request.Service == "screen" {
				if presentation == nil {
					presentation, err = s.startPresentation(connection, ctx)
				}
				if err != nil {
					rpcError(connection, ctx, request, err)
					continue
				}
				result, callErr := presentation.call(ctx, request)
				if callErr != nil {
					rpcError(connection, ctx, request, callErr)
				} else {
					rpcResult(connection, ctx, request, result)
				}
				continue
			}
			s.handleRPC(connection, ctx, agents, request, body)
		case "net":
			if !socketAllows(authentication.capabilities, "net."+request.ProtocolName+".connect") && !socketAllows(authentication.capabilities, "net.protocol."+request.ProtocolName) {
				sendError(connection, ctx, request.ID, permissionDeniedError(networkCapability(request.ProtocolName)))
				continue
			}
			s.handleNetwork(connection, ctx, bridges, request, body)
		case "net-data":
			s.handleNetwork(connection, ctx, bridges, message{Action: "data", BridgeID: request.BridgeID}, body)
		case "net-close":
			s.handleNetwork(connection, ctx, bridges, message{Action: "close", BridgeID: request.BridgeID}, nil)
		default:
			sendError(connection, ctx, request.ID, "Unknown remote request")
		}
	}
}

// fsRemoteExcluded lists filesystem capabilities the umbrella `fs.remote`
// permission never implies: they run programs or open files in other apps.
var fsRemoteExcluded = map[string]bool{"fs.exec": true, "fs.execFile": true, "fs.execTerminal": true, "fs.openPath": true, "fs.openWith": true}

func socketAllows(allowed []string, required string) bool {
	if required == "" {
		return true
	}
	if allowed == nil {
		return false
	}
	for _, value := range allowed {
		if value == required {
			return true
		}
		if value == "fs.remote" && strings.HasPrefix(required, "fs.") && !fsRemoteExcluded[required] {
			return true
		}
	}
	return false
}

func sendFrameError(c protocolSocket, ctx context.Context, request message, text string) {
	switch request.Type {
	case "exec":
		send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": text})
	case "rpc":
		rpcError(c, ctx, request, errors.New(text))
	case "net":
		send(c, ctx, map[string]any{"type": "net-error", "id": request.ID, "error": text})
	default:
		sendError(c, ctx, request.ID, text)
	}
}

// networkCapability names the permission a `net` frame needs for error text.
func networkCapability(protocol string) string {
	switch protocol {
	case "tcp", "udp":
		return "net." + protocol + ".connect"
	}
	return "net.protocol." + protocol
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func sameCapabilitySet(left, right []string) bool {
	normalize := func(values []string) []string {
		seen := map[string]struct{}{}
		out := make([]string, 0, len(values))
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(normalize(left), normalize(right))
}
func filesystemCapability(method string) string {
	switch method {
	case "readChunkBinary":
		return "fs.readChunk"
	case "writeChunkBinary":
		return "fs.writeChunk"
	case "packZip":
		return "fs.pack"
	case "openWithOptions":
		return "fs.openWith"
	case "watchSnapshot":
		return "fs.watch"
	}
	return "fs." + method
}
func rpcCapability(request message) string {
	switch request.Service {
	case "screen":
		return "screen.capture"
	case "tray":
		return "tray.manage"
	case "globalShortcut":
		return "globalShortcut"
	case "agent":
		return "agent.session"
	case "calendar":
		return "calendar.read"
	case "fileSearch":
		return "search.files"
	case "secrets":
		return "secrets.manage"
	case "ftp", "sftp":
		return "net.protocol." + request.Service
	case "sessions":
		return "sessions.manage"
	case "system":
		if request.Method == "terminate" {
			return "system.processes.terminate"
		}
		return "system.ports.read"
	case "http":
		if parsed, err := url.Parse(stringValue(objectArg(request.Args, 0)["url"])); err == nil {
			return "net.protocol." + parsed.Scheme
		}
		return "net.protocol.https"
	case "associations":
		return "fileAssociations.manage"
	case "fileImport":
		return "file.import"
	case "clipboard":
		if request.Method == "readText" || request.Method == "readImage" || request.Method == "readFiles" {
			return "clipboard.read"
		}
		return "clipboard.write"
	case "externalOpen":
		return "externalOpen.files"
	case "apps":
		if request.Method == "publish" {
			return "apps.publish"
		}
		return "apps.manage"
	case "lan":
		return "remoteEnv.manage"
	case "permissions":
		return permissionsManage
	default:
		return request.Service
	}
}

func capabilities() []string {
	result := []string{
		"fs.remote", "fs.exec", "fs.execFile", "fs.home", "fs.roots", "fs.list", "fs.stat", "fs.readText", "fs.writeText", "fs.readBase64", "fs.writeBase64", "fs.mkdir", "fs.copy", "fs.move", "fs.remove", "fs.dirSize", "fs.diskUsage", "fs.readChunk", "fs.writeChunk", "fs.trash", "fs.openPath", "fs.openWith", "fs.execTerminal", "fs.pack", "fs.watch",
		"net.tcp.connect", "net.udp.connect", "net.protocol.http", "net.protocol.https", "net.protocol.ftp", "net.protocol.sftp", "net.protocol.rdp", "agent.session", "calendar.read", "search.files", "secrets.manage", "sessions.manage", "system.ports.read", "system.processes.terminate", "fileAssociations.manage", "externalOpen.files", "file.import", "clipboard.read", "clipboard.write", "apps.manage", "apps.publish",
	}
	if presentationSupported() {
		result = append(result, "tray.manage", "globalShortcut")
	}
	if screenCaptureSupported() {
		result = append(result, "screen.capture")
	}
	return result
}

func send(c protocolSocket, ctx context.Context, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if cipherState, ok := connectionE2EE.Load(c); ok {
		data, err = sealRemoteFrame(cipherState.(relayCipher).tokenHash, data)
		if err != nil {
			return false
		}
	}
	writer, _ := connectionWriters.LoadOrStore(c, &sync.Mutex{})
	writer.(*sync.Mutex).Lock()
	defer writer.(*sync.Mutex).Unlock()
	return c.Write(ctx, websocket.MessageText, data) == nil
}

// DFB1 is the shared DynApp binary-frame envelope. Keeping it byte-for-byte
// compatible avoids base64 expansion for large PWA file transfers.
func sendBinary(c protocolSocket, ctx context.Context, header any, body []byte) bool {
	headerJSON, err := json.Marshal(header)
	if err != nil || len(headerJSON) > 512*1024 {
		return false
	}
	frame := make([]byte, 8+len(headerJSON)+len(body))
	copy(frame[:4], []byte("DFB1"))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerJSON)))
	copy(frame[8:], headerJSON)
	copy(frame[8+len(headerJSON):], body)
	if cipherState, ok := connectionE2EE.Load(c); ok {
		var err error
		frame, err = sealRemoteFrame(cipherState.(relayCipher).tokenHash, frame)
		if err != nil {
			return false
		}
		// E2EE envelopes are JSON records even when their protected payload is
		// a DFB1 binary frame.
		writer, _ := connectionWriters.LoadOrStore(c, &sync.Mutex{})
		writer.(*sync.Mutex).Lock()
		defer writer.(*sync.Mutex).Unlock()
		return c.Write(ctx, websocket.MessageText, frame) == nil
	}
	writer, _ := connectionWriters.LoadOrStore(c, &sync.Mutex{})
	writer.(*sync.Mutex).Lock()
	defer writer.(*sync.Mutex).Unlock()
	return c.Write(ctx, websocket.MessageBinary, frame) == nil
}

const remoteE2EEContext = "dynapp-remote-e2ee-v1"

func remoteKeyID(tokenHash string) string {
	sum := sha256.Sum256([]byte(remoteE2EEContext + ":id:" + tokenHash))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func remoteTrafficKey(tokenHash string) []byte {
	sum := sha256.Sum256([]byte(remoteE2EEContext + ":key:" + tokenHash))
	return sum[:]
}
func sealRemoteFrame(tokenHash string, payload []byte) ([]byte, error) {
	block, err := aes.NewCipher(remoteTrafficKey(tokenHash))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, payload, nil)
	ciphertext, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	return json.Marshal(map[string]any{"type": "dynapp-e2ee", "version": 1, "nonce": base64.RawURLEncoding.EncodeToString(nonce), "ciphertext": base64.RawURLEncoding.EncodeToString(ciphertext), "tag": base64.RawURLEncoding.EncodeToString(tag)})
}
func openRemoteFrame(tokenHash string, wire []byte) ([]byte, error) {
	var envelope struct {
		Type                   string `json:"type"`
		Version                int    `json:"version"`
		Nonce, Ciphertext, Tag string
	}
	if err := json.Unmarshal(wire, &envelope); err != nil || envelope.Type != "dynapp-e2ee" || envelope.Version != 1 {
		return nil, errors.New("end-to-end encrypted remote frame is invalid")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, errors.New("end-to-end encrypted remote frame is invalid")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, errors.New("end-to-end encrypted remote frame is invalid")
	}
	tag, err := base64.RawURLEncoding.DecodeString(envelope.Tag)
	if err != nil {
		return nil, errors.New("end-to-end encrypted remote frame is invalid")
	}
	block, err := aes.NewCipher(remoteTrafficKey(tokenHash))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() || len(tag) != gcm.Overhead() || len(encoded) == 0 {
		return nil, errors.New("end-to-end encrypted remote frame is invalid")
	}
	return gcm.Open(nil, nonce, append(encoded, tag...), nil)
}

func decodeBinaryFrame(frame []byte) (message, []byte, error) {
	if len(frame) < 8 || string(frame[:4]) != "DFB1" {
		return message{}, nil, errors.New("invalid binary frame")
	}
	headerLength := int(binary.BigEndian.Uint32(frame[4:8]))
	if headerLength < 1 || headerLength > len(frame)-8 {
		return message{}, nil, errors.New("invalid binary frame")
	}
	var header message
	if err := json.Unmarshal(frame[8:8+headerLength], &header); err != nil {
		return message{}, nil, err
	}
	return header, frame[8+headerLength:], nil
}

func protocolNumber(value json.RawMessage) int {
	var number int
	_ = json.Unmarshal(value, &number)
	return number
}

func protocolFeatures(version int, reliableStreams ...bool) map[string]bool {
	if version < ProtocolVersion {
		return map[string]bool{}
	}
	// A loopback WebSocket has multiplexed logical bridge channels but no QUIC
	// reliable stream. The PWA therefore keeps bridge traffic in the shared
	// record socket, exactly as it does for relay WebSockets.
	return map[string]bool{"multiplexedChannels": true, "reliableStreams": len(reliableStreams) > 0 && reliableStreams[0]}
}
func protocolString(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}

func sendError(c protocolSocket, ctx context.Context, id, text string) {
	send(c, ctx, map[string]any{"type": "fs-error", "id": id, "error": text})
}
func fsResult(c protocolSocket, ctx context.Context, id string, result any) {
	send(c, ctx, map[string]any{"type": "fs-result", "id": id, "result": result})
}

func (s *Server) handleFilesystem(c protocolSocket, ctx context.Context, request message, body []byte) {
	if request.ID == "" {
		sendError(c, ctx, "", "Remote filesystem request is invalid")
		return
	}
	result, err := filesystem(request.Method, request.Args, body)
	if err != nil {
		sendError(c, ctx, request.ID, err.Error())
		return
	}
	if request.Method == "readBase64" || request.Method == "readChunk" || request.Method == "readChunkBinary" {
		response, ok := result.(map[string]any)
		if !ok {
			sendError(c, ctx, request.ID, "Remote filesystem response is invalid")
			return
		}
		encoded, _ := response["base64"].(string)
		bytes, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			sendError(c, ctx, request.ID, "Remote filesystem response is invalid")
			return
		}
		delete(response, "base64")
		if request.Method == "readChunkBinary" {
			response["binary"] = true
		}
		sendBinary(c, ctx, map[string]any{"type": "fs-result", "id": request.ID, "result": response}, bytes)
		return
	}
	fsResult(c, ctx, request.ID, result)
}

func filesystem(method string, args []any, body []byte) (any, error) {
	pathArg := func(index int, label string) (string, error) {
		if len(args) <= index {
			return "", fmt.Errorf("%s is invalid", label)
		}
		value, ok := args[index].(string)
		if !ok || strings.TrimSpace(value) == "" || strings.Contains(value, "\x00") {
			return "", fmt.Errorf("%s is invalid", label)
		}
		return value, nil
	}
	switch method {
	case "home":
		return userHome(), nil
	case "roots":
		return filesystemRoots()
	case "list":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return listDir(value)
	case "stat":
		value, err := pathArg(0, "Path")
		if err != nil {
			return nil, err
		}
		return statPath(value)
	case "readText":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readText(value, numberArg(args, 1, 2_000_000, maxReadBytes))
	case "writeText":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		content := stringArg(args, 1)
		return nil, os.WriteFile(value, []byte(content), 0o666)
	case "readBase64":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readBase64(value, numberArg(args, 1, maxReadBytes, maxReadBytes))
	case "writeBase64":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		raw := body
		if raw == nil {
			decoded, decodeErr := base64.StdEncoding.DecodeString(stringArg(args, 1))
			if decodeErr != nil {
				return nil, errors.New("Base64 data is invalid")
			}
			raw = decoded
		}
		if raw == nil {
			return nil, errors.New("Base64 data is invalid")
		}
		if len(raw) > maxWriteBytes {
			return nil, errors.New("Remote filesystem write is too large")
		}
		err = os.WriteFile(value, raw, 0o666)
		return map[string]any{"path": value, "size": len(raw)}, err
	case "mkdir":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return nil, os.MkdirAll(value, 0o777)
	case "copy":
		source, err := pathArg(0, "Source path")
		if err != nil {
			return nil, err
		}
		target, err := pathArg(1, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, copyPath(source, target)
	case "move":
		source, err := pathArg(0, "Source path")
		if err != nil {
			return nil, err
		}
		target, err := pathArg(1, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, os.Rename(source, target)
	case "remove":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, os.RemoveAll(value)
	case "dirSize":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return dirSize(value)
	case "diskUsage":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return diskUsage(value)
	case "readChunk", "readChunkBinary":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readChunk(value, numberArg(args, 1, 0, int(^uint(0)>>1)), numberArg(args, 2, 1_000_000, maxReadBytes))
	case "writeChunk", "writeChunkBinary":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		offset := numberArg(args, 1, 0, int(^uint(0)>>1))
		raw := body
		if raw == nil {
			decoded, decodeErr := base64.StdEncoding.DecodeString(stringArg(args, 2))
			if decodeErr != nil {
				return nil, errors.New("Base64 data is invalid")
			}
			raw = decoded
		}
		if raw == nil {
			return nil, errors.New("Base64 data is invalid")
		}
		if len(raw) > maxChunkBytes {
			return nil, errors.New("Remote filesystem write chunk is too large")
		}
		if err = writeChunk(value, int64(offset), raw, boolArg(args, 3)); err != nil {
			return nil, err
		}
		return map[string]any{"path": value, "offset": offset, "bytesWritten": len(raw)}, nil
	case "trash":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, trashPath(value)
	case "openPath":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, openDesktopPath(value)
	case "openWithOptions":
		return desktopOpenWithOptions(), nil
	case "openWith":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, openDesktopPathWith(value, stringArg(args, 1))
	case "execTerminal":
		return nil, openTerminal(stringArg(args, 0), stringArg(args, 1))
	case "packZip":
		if len(args) < 2 {
			return nil, errors.New("archive request is invalid")
		}
		var sources []string
		if raw, ok := args[0].([]any); ok {
			for _, item := range raw {
				if value := stringValue(item); value != "" {
					sources = append(sources, value)
				}
			}
		}
		target, err := pathArg(1, "Archive path")
		if err != nil {
			return nil, err
		}
		return nil, createZip(sources, target, objectArg(args, 2))
	case "watchSnapshot":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return watchSnapshot(value)
	default:
		return nil, errors.New("Remote filesystem method is not supported")
	}
}

func (s *Server) handleExec(c protocolSocket, ctx context.Context, processes *processSet, request message) {
	if strings.TrimSpace(request.Command) != "" {
		if runtime.GOOS == "windows" {
			request.File, request.Args = "cmd.exe", []any{"/d", "/s", "/c", request.Command}
		} else {
			request.File, request.Args = "/bin/sh", []any{"-c", request.Command}
		}
	}
	if request.ID == "" || strings.TrimSpace(request.File) == "" || len(request.File) > maxExecArgumentBytes || len(request.Command) > maxExecArgumentBytes || len(request.Cwd) > maxExecArgumentBytes || len(request.Args) > maxExecArgs {
		send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": "Executable request is invalid"})
		return
	}
	args := make([]string, len(request.Args))
	for i, value := range request.Args {
		text, ok := value.(string)
		if !ok || len(text) > maxExecArgumentBytes {
			send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": "Executable request is invalid"})
			return
		}
		args[i] = text
	}
	if request.Stream {
		processes.start(request, args)
		return
	}
	execContext, cancel := context.WithTimeout(ctx, maxExecDuration)
	defer cancel()
	command := exec.CommandContext(execContext, request.File, args...)
	if request.Cwd != "" {
		command.Dir = request.Cwd
	}
	if err := applyExecEnv(command, request.Env); err != nil {
		send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": err.Error()})
		return
	}
	var stdout, stderr boundedExecBuffer
	stdout.limit, stderr.limit = maxExecOutputBytes, maxExecOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	send(c, ctx, map[string]any{"type": "exec-start", "id": request.ID})
	err := command.Run()
	if stdout.Len() > 0 {
		send(c, ctx, map[string]any{"type": "exec-output", "id": request.ID, "stream": "stdout", "data": stdout.String()})
	}
	if stderr.Len() > 0 {
		send(c, ctx, map[string]any{"type": "exec-output", "id": request.ID, "stream": "stderr", "data": stderr.String()})
	}
	code := 0
	if execContext.Err() == context.DeadlineExceeded {
		send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": "Remote command timed out"})
		return
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			send(c, ctx, map[string]any{"type": "exec-error", "id": request.ID, "error": err.Error()})
			return
		}
	}
	send(c, ctx, map[string]any{"type": "exec-exit", "id": request.ID, "code": code, "signal": nil})
}
