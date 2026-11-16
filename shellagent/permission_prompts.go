package shellagent

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// The permission prompt is one agent-owned page. The macOS and Windows native
// hosts show it in a web view of their own, separate from the app's page, so
// every native permission decision (first launch, update, review, and a call
// that needs more access) looks and behaves the same on both systems. Hosts
// that predate the page ignore the "page" field and draw their own dialog
// from "groups".
//
//go:embed native_permission_prompt.html
var nativePermissionPromptTemplate string

// requiredPermissionTimeout bounds how long a call waits for the user.
const requiredPermissionTimeout = 5 * time.Minute

// nativePermissionPage fills the prompt template. json.Marshal escapes <, >
// and &, so the request cannot end the script element it is written into.
func nativePermissionPage(kind, appName, origin string, groups []map[string]any, focus []string) string {
	platform := "darwin"
	if nativePlatformName() == "windows" {
		platform = "windows"
	}
	if focus == nil {
		focus = []string{}
	}
	payload, err := json.Marshal(map[string]any{
		"kind": kind, "appName": appName, "origin": origin, "groups": groups,
		"focus": focus, "platform": platform,
	})
	if err != nil {
		return ""
	}
	return strings.Replace(nativePermissionPromptTemplate, "__DYNAPP_PERMISSION_REQUEST__", string(payload), 1)
}

// nativePermissionRequestFrame is the native-host-permission-request frame
// for one decision. "required" is new; older hosts show it as "delta".
func nativePermissionRequestFrame(requestID, kind, appName, origin string, groups []map[string]any, focus []string) map[string]any {
	hostKind := kind
	if kind == "required" {
		hostKind = "delta"
	}
	frame := map[string]any{
		"type": "native-host-permission-request", "requestId": requestID, "kind": hostKind,
		"appName": appName, "origin": origin, "groups": groups,
	}
	if page := nativePermissionPage(kind, appName, origin, groups, focus); page != "" {
		frame["page"] = page
	}
	return frame
}

// permissionDemand is one in-flight "this call needs more access" decision,
// shared by every call that needs the same group of the same app.
type permissionDemand struct {
	done    chan struct{}
	granted []string
}

type permissionDemands struct {
	mu       sync.Mutex
	inflight map[string]*permissionDemand
	// declined remembers groups the user refused while the agent runs, so a
	// retrying app cannot keep raising the prompt.
	declined map[string]bool
}

func (s *Server) demands() *permissionDemands {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permissionDemandState == nil {
		s.permissionDemandState = &permissionDemands{inflight: map[string]*permissionDemand{}, declined: map[string]bool{}}
	}
	return s.permissionDemandState
}

// permissionGroupOf returns the catalog group a permission belongs to.
func permissionGroupOf(permission string) string {
	for _, descriptor := range describePermissions([]string{permission}) {
		if descriptor.Permission == permission {
			return descriptor.Group
		}
	}
	return permission
}

// ensureCapability is the one place a refused call can turn into a prompt.
// It returns true when auth now holds required. Only a permission the app's
// manifest declares can be asked for; the manifest stays the ceiling.
//
// For a native app the user answers in the app's own window and the call
// continues. For a browser app the decision belongs to Dyner: the agent
// registers a pending request and the call is refused for now.
func (s *Server) ensureCapability(ctx context.Context, auth *socketAuthentication, required string) bool {
	if auth == nil || required == "" {
		return true
	}
	if socketAllows(auth.capabilities, required) {
		return true
	}
	if auth.scoped || auth.storeID == "" || required == permissionsManage || !stringSliceHas(auth.declared, required) {
		return false
	}
	if auth.native {
		return s.ensureNativeCapability(ctx, auth, required)
	}
	if auth.keyID != "" {
		s.requestBrowserCapability(auth, required)
	}
	return false
}

// requireCapability is ensureCapability for handlers that only hold the
// request: crossovers such as http.download needing fs.writeBase64.
func (s *Server) requireCapability(ctx context.Context, request message, required string) bool {
	if request.auth == nil {
		return true
	}
	return s.ensureCapability(ctx, request.auth, required)
}

func (s *Server) ensureNativeCapability(ctx context.Context, auth *socketAuthentication, required string) bool {
	app, ok := s.nativeApp(auth.storeID)
	if !ok {
		return false
	}
	// Granted meanwhile (another window, Dyner, the Permissions menu).
	if socketAllows(app.Capabilities, required) {
		s.widenSocketGrant(auth, app.Capabilities)
		return true
	}
	group := permissionGroupOf(required)
	key := auth.storeID + "\x00" + group
	state := s.demands()
	state.mu.Lock()
	if state.declined[key] {
		state.mu.Unlock()
		return false
	}
	demand := state.inflight[key]
	owner := demand == nil
	if owner {
		demand = &permissionDemand{done: make(chan struct{})}
		state.inflight[key] = demand
	}
	state.mu.Unlock()
	if owner {
		granted, approved := s.askNativeRequired(ctx, app, auth, group, required)
		demand.granted = granted
		state.mu.Lock()
		delete(state.inflight, key)
		if !approved || !socketAllows(granted, required) {
			state.declined[key] = true
		}
		state.mu.Unlock()
		close(demand.done)
	} else {
		select {
		case <-demand.done:
		case <-ctx.Done():
			return false
		}
	}
	if !socketAllows(demand.granted, required) {
		return false
	}
	if updated, ok := s.nativeApp(auth.storeID); ok {
		s.widenSocketGrant(auth, updated.Capabilities)
	}
	return true
}

// askNativeRequired shows the prompt for the declared, ungranted permissions
// in required's group and stores the answer.
func (s *Server) askNativeRequired(ctx context.Context, app NativeApp, auth *socketAuthentication, group, required string) ([]string, bool) {
	ask := []string{}
	for _, permission := range auth.declared {
		if permission == permissionsManage || socketAllows(app.Capabilities, permission) {
			continue
		}
		if permission == required || permissionGroupOf(permission) == group {
			ask = append(ask, permission)
		}
	}
	ask = normalizePermissionSet(ask)
	control := s.nativeControl(app.StoreID)
	if control == nil || len(ask) == 0 {
		return nil, false
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: "required", AppName: auth.appName, StoreID: app.StoreID, Origin: app.Origin, Declared: ask,
	})
	if err != nil {
		return nil, false
	}
	name := auth.appName
	if name == "" {
		name = app.Name
	}
	reasons := map[string]string{}
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	if manifest, err := s.resolveNativeManifest(resolveCtx, config, app.StoreID); err == nil {
		reasons = manifest.reasons
	}
	cancel()
	groups := nativePermissionGroups(ask, app.Capabilities, reasons)
	if !send(control.socket, ctx, nativePermissionRequestFrame(pending.ID, "required", name, app.Origin, groups, []string{group})) {
		s.unregisterPending(pending.ID)
		return nil, false
	}
	waitCtx, stop := context.WithTimeout(ctx, requiredPermissionTimeout)
	defer stop()
	granted, approved := s.awaitDecision(waitCtx, pending)
	if !approved || !pending.wasResolved() {
		return nil, false
	}
	granted = normalizePermissionSet(intersectStrings(granted, ask))
	if len(granted) == 0 {
		return granted, true
	}
	updated, ok := s.nativeApp(app.StoreID)
	if !ok {
		return nil, false
	}
	updated.Capabilities = normalizePermissionSet(unionStrings(updated.Capabilities, granted))
	updated.Declared = normalizePermissionSet(unionStrings(updated.Declared, ask))
	if updated.ApprovedAt == 0 {
		updated.ApprovedAt = time.Now().UnixMilli()
	}
	s.upsertNativeApp(updated)
	go s.notifyPermissionsChanged()
	go s.pushCapabilities(updated.StoreID, nativeKeyID(updated.StoreID), updated.Capabilities)
	return granted, true
}

// widenSocketGrant adds stored grants to a live socket, inside its declared
// ceiling. It replaces the slices instead of editing them in place.
func (s *Server) widenSocketGrant(auth *socketAuthentication, stored []string) {
	effective := normalizePermissionSet(unionStrings(auth.capabilities, intersectStrings(stored, auth.declared)))
	effective = subtractStrings(effective, []string{permissionsManage})
	if stringSliceHas(auth.capabilities, permissionsManage) {
		effective = unionStrings(effective, []string{permissionsManage})
	}
	auth.capabilities = effective
	auth.stored = normalizePermissionSet(unionStrings(auth.stored, stored))
}

// pushCapabilities tells every open page of an app what its grant is now, so
// the runtime stops refusing those calls before they reach the agent.
func (s *Server) pushCapabilities(storeID, keyID string, stored []string) {
	state := s.permissions()
	state.mu.Lock()
	type target struct {
		socket       protocolSocket
		capabilities []string
	}
	targets := []target{}
	for socket, auth := range state.live {
		if auth.storeID != storeID || (keyID != "" && auth.keyID != keyID) {
			continue
		}
		effective := normalizePermissionSet(unionStrings(auth.capabilities, intersectStrings(stored, auth.declared)))
		targets = append(targets, target{socket, effective})
	}
	state.mu.Unlock()
	for _, target := range targets {
		send(target.socket, context.Background(), map[string]any{"type": "capabilities", "capabilities": target.capabilities})
	}
}

// requestBrowserCapability registers one pending "required" request for a
// browser app and lets Dyner decide it. It never blocks the call.
func (s *Server) requestBrowserCapability(auth *socketAuthentication, required string) {
	group := permissionGroupOf(required)
	key := "browser\x00" + auth.keyID + "\x00" + auth.storeID + "\x00" + group
	state := s.demands()
	state.mu.Lock()
	if state.declined[key] || state.inflight[key] != nil {
		state.mu.Unlock()
		return
	}
	demand := &permissionDemand{done: make(chan struct{})}
	state.inflight[key] = demand
	state.mu.Unlock()
	ask := []string{}
	for _, permission := range auth.declared {
		if permission == permissionsManage || socketAllows(auth.capabilities, permission) {
			continue
		}
		if permission == required || permissionGroupOf(permission) == group {
			ask = append(ask, permission)
		}
	}
	finish := func(declined bool) {
		state.mu.Lock()
		delete(state.inflight, key)
		if declined {
			state.declined[key] = true
		}
		state.mu.Unlock()
		close(demand.done)
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: "required", AppName: auth.appName, StoreID: auth.storeID, Origin: auth.origin,
		Development: auth.development, Declared: normalizePermissionSet(ask),
	})
	if err != nil {
		finish(false)
		return
	}
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	s.announcePending(auth.keyID, auth.storeID, config, pending)
	keyID, storeID, origin := auth.keyID, auth.storeID, auth.origin
	go func() {
		granted, approved := s.awaitDecision(context.Background(), pending)
		resolved := pending.wasResolved()
		if approved && len(granted) > 0 {
			s.applyBrowserGrant(keyID, origin, storeID, granted)
		}
		finish(resolved && (!approved || !stringSliceHas(granted, required)))
	}()
}

// announcePending tells the app's open pages that Dyner has a request to
// decide, so the runtime can show where to answer it.
func (s *Server) announcePending(keyID, storeID string, config Config, pending *pendingRequest) {
	state := s.permissions()
	state.mu.Lock()
	targets := []protocolSocket{}
	for socket, auth := range state.live {
		if auth.keyID == keyID && auth.storeID == storeID {
			targets = append(targets, socket)
		}
	}
	state.mu.Unlock()
	frame := map[string]any{
		"type": "permission-pending", "requestId": pending.ID, "kind": pending.Kind,
		"permissions": pending.Declared, "reviewUrl": config.reviewURL(pending.ID),
		"expiresInMs": time.Until(pending.ExpiresAt).Milliseconds(),
	}
	for _, socket := range targets {
		send(socket, context.Background(), frame)
	}
}

// applyBrowserGrant adds a Dyner decision to a browser identity and pushes
// the new set to its open pages.
func (s *Server) applyBrowserGrant(keyID, origin, storeID string, granted []string) {
	identity, ok := s.findIdentity(keyID, origin, storeID)
	if !ok {
		return
	}
	identity.Capabilities = normalizePermissionSet(unionStrings(identity.Capabilities, granted))
	identity.Declared = normalizePermissionSet(unionStrings(identity.Declared, granted))
	s.upsertBrowserIdentity(identity)
	go s.notifyPermissionsChanged()
	s.pushCapabilities(storeID, keyID, identity.Capabilities)
}

// startBrowserDelta asks Dyner about permissions an app update added since
// this browser identity was approved. The app keeps running meanwhile.
func (s *Server) startBrowserDelta(identity BrowserIdentity, resolved resolvedApp, fresh []string, config Config) {
	fresh = normalizePermissionSet(subtractStrings(fresh, []string{permissionsManage}))
	if len(fresh) == 0 || s.autoApprovePairings {
		return
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: "delta", AppName: resolved.Name, StoreID: resolved.StoreID, Origin: identity.Origin,
		Development: resolved.Development, Declared: fresh,
	})
	if err != nil {
		return
	}
	go func() {
		// Let the hello reach the page first so its runtime is listening.
		time.Sleep(300 * time.Millisecond)
		s.announcePending(identity.KeyID, identity.StoreID, config, pending)
	}()
	go func() {
		granted, approved := s.awaitDecision(context.Background(), pending)
		if approved && len(granted) > 0 {
			s.applyBrowserGrant(identity.KeyID, identity.Origin, identity.StoreID, granted)
		}
	}()
}

// nativeControl returns the control channel of a running native app.
func (s *Server) nativeControl(storeID string) *automationControl {
	s.automation.mu.Lock()
	defer s.automation.mu.Unlock()
	control := s.automation.controls[storeID]
	if control == nil {
		return nil
	}
	control.mu.Lock()
	closed := control.closed
	control.mu.Unlock()
	if closed {
		return nil
	}
	return control
}

// routeNativeDecision resolves a pending request from a host's decision frame.
// Only requests about the host's own app are accepted.
func (s *Server) routeNativeDecision(storeID string, frame nativeHostFrame) bool {
	if frame.Type != "native-host-permission-decision" || frame.RequestID == "" {
		return false
	}
	state := s.permissions()
	state.mu.Lock()
	pending := state.pending[frame.RequestID]
	state.mu.Unlock()
	if pending == nil || pending.StoreID != storeID {
		return true
	}
	pending.resolve(intersectStrings(frame.Capabilities, pending.Declared), frame.Capabilities != nil)
	return true
}
