package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// A platform channel is how a host that owns the system UI performs native
// installs for the agent. Android is the only such host: its app process owns
// the launcher shortcuts and activities, so the agent asks it to pin, remove,
// and open apps instead of writing bundles itself (docs/native-host.md).
//
// The host opens one connection with
// {"type":"native-host-hello","version":1,"purpose":"platform"}. The agent
// answers {"type":"native-platform-ready","version":1,"platform","catalog"} and
// then sends {"type":"native-platform-request","id","op","args"}; the host
// replies {"type":"native-platform-result","id","result"} or with "error".

const nativePlatformCallTimeout = 2 * time.Minute

// nativePlatformChannelEnabled is replaceable so tests can drive the platform
// channel on any OS.
var nativePlatformChannelEnabled = nativePlatformUsesHostChannel

var activeNativePlatform atomic.Pointer[nativePlatformConn]

type nativePlatformConn struct {
	socket  *nativeFrameSocket
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan nativePlatformResult
	closed  chan struct{}
}

type nativePlatformResult struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

var errNativePlatformUnavailable = errors.New("the DynApp app is not connected to the agent")

// callNativePlatform sends one request to the connected platform host and
// waits for its answer.
func callNativePlatform(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	host := activeNativePlatform.Load()
	if host == nil {
		return nil, errNativePlatformUnavailable
	}
	return host.call(ctx, op, args)
}

func (host *nativePlatformConn) call(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	host.mu.Lock()
	host.nextID++
	id := fmt.Sprintf("p%d", host.nextID)
	reply := make(chan nativePlatformResult, 1)
	host.pending[id] = reply
	host.mu.Unlock()
	defer func() {
		host.mu.Lock()
		delete(host.pending, id)
		host.mu.Unlock()
	}()
	if args == nil {
		args = map[string]any{}
	}
	callCtx, cancel := context.WithTimeout(ctx, nativePlatformCallTimeout)
	defer cancel()
	if err := host.socket.Write(callCtx, websocket.MessageText, mustJSON(map[string]any{
		"type": "native-platform-request", "id": id, "op": op, "args": args,
	})); err != nil {
		return nil, errNativePlatformUnavailable
	}
	select {
	case result := <-reply:
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return result.Result, nil
	case <-host.closed:
		return nil, errNativePlatformUnavailable
	case <-callCtx.Done():
		return nil, fmt.Errorf("the DynApp app did not answer %s in time", op)
	}
}

// serveNativePlatform owns the platform connection until it closes. A newer
// connection replaces an older one, so a restarted host process takes over.
func (s *Server) serveNativePlatform(ctx context.Context, socket *nativeFrameSocket) {
	host := &nativePlatformConn{socket: socket, pending: map[string]chan nativePlatformResult{}, closed: make(chan struct{})}
	if previous := activeNativePlatform.Swap(host); previous != nil {
		_ = previous.socket.conn.Close()
	}
	defer func() {
		activeNativePlatform.CompareAndSwap(host, nil)
		close(host.closed)
	}()
	catalog, err := s.ensureNativeCatalogApp()
	if err != nil {
		log.Printf("DynApp Shell agent: could not register the catalog app: %v", err)
	}
	if !send(socket, ctx, map[string]any{
		"type": "native-platform-ready", "version": nativeProtocolVersion,
		"platform": nativePlatformName(), "catalog": catalog,
	}) {
		return
	}
	for {
		messageType, data, err := socket.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var result nativePlatformResult
		if json.Unmarshal(data, &result) != nil || result.Type != "native-platform-result" {
			continue
		}
		host.mu.Lock()
		reply := host.pending[result.ID]
		host.mu.Unlock()
		if reply != nil {
			select {
			case reply <- result:
			default:
			}
		}
	}
}

// ensureNativeCatalogApp records the Dyner catalog as an installed native app
// with permission authority. A platform host shows the catalog as its own
// launcher entry, so there is no browser in which to install it first.
func (s *Server) ensureNativeCatalogApp() (map[string]any, error) {
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	storeID := config.catalogAppID()
	if !validStoreID(storeID) {
		return nil, errors.New("the catalog app id is invalid")
	}
	app, installed := s.nativeApp(storeID)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	manifest, err := s.resolveNativeManifest(ctx, config, storeID)
	cancel()
	origin, name := app.Origin, app.Name
	if err == nil {
		origin, name = manifest.origin, manifest.app.Name
	}
	if origin == "" {
		origin = legacyHostedOrigin(config, storeID)
	}
	if name == "" {
		name = "Dyner"
	}
	if origin == "" {
		return nil, errors.New("the catalog has no hosted origin")
	}
	declared := []string{permissionsManage}
	if err == nil {
		declared = normalizePermissionSet(unionStrings(manifest.app.Declared, declared))
	} else if installed {
		declared = normalizePermissionSet(unionStrings(app.Capabilities, declared))
	}
	startURL := app.URL
	if startURL == "" || app.Origin != origin {
		startURL = origin + "/"
	}
	now := time.Now().UnixMilli()
	updated := NativeApp{
		StoreID: storeID, Name: name, Origin: origin, URL: startURL, Path: nativeCatalogPath,
		InstalledAt: now, ApprovedAt: now, Authority: true,
		Capabilities: declared, Declared: append([]string(nil), declared...),
	}
	if err == nil {
		updated.Connect = manifest.app.Connect
	} else {
		updated.Connect = app.Connect
	}
	if installed && app.InstalledAt != 0 {
		updated.InstalledAt = app.InstalledAt
	}
	if !installed || app.Origin != updated.Origin || app.Name != updated.Name || !sameCapabilitySet(app.Capabilities, updated.Capabilities) || app.Path != updated.Path {
		s.upsertNativeApp(updated)
	}
	return map[string]any{"storeId": storeID, "name": name, "origin": origin, "url": startURL}, nil
}

// nativeCatalogPath marks the catalog record a platform host owns.
const nativeCatalogPath = "platform:catalog"
