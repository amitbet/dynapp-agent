package shellagent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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

// maxLaunchDismissals is how many launch prompts in a row may be closed
// without an answer before the agent stops asking at launch. The request then
// waits in the app's "Permissions needed" chip.
const maxLaunchDismissals = 3

// launchPermissionAsk decides what a native app's launch prompt asks for.
// Explicit refusals and "Don't ask again" are not asked at launch; a prompt
// that was only dismissed comes back, up to maxLaunchDismissals times.
func launchPermissionAsk(app NativeApp, declared []string) (string, []string) {
	if app.Dismissals >= maxLaunchDismissals {
		return "", nil
	}
	if app.ApprovedAt == 0 {
		ask := normalizePermissionSet(subtractStrings(declared, app.DontAsk))
		if len(ask) == 0 && len(declared) > 0 {
			return "", nil
		}
		return "pairing", ask
	}
	fresh := subtractStrings(declared, unionStrings(unionStrings(app.Declared, app.Denied), unionStrings(app.DontAsk, app.Capabilities)))
	if len(fresh) == 0 {
		return "", nil
	}
	return "delta", normalizePermissionSet(fresh)
}

// applyNativeDecision stores one answer about ask. Allow grants the checked
// permissions and refuses the unchecked ones; deny refuses all of ask;
// dismissed changes nothing except the dismissal count. A grant clears a
// permission's refusal and "Don't ask again".
func applyNativeDecision(app NativeApp, kind string, ask, granted []string, outcome string, dontAsk bool) NativeApp {
	ask = normalizePermissionSet(ask)
	if outcome == decisionDismissed {
		if kind == "pairing" || kind == "delta" {
			app.Dismissals++
		}
		return app
	}
	app.Dismissals = 0
	granted = normalizePermissionSet(intersectStrings(granted, ask))
	refused := ask
	if outcome == decisionAllow {
		refused = subtractStrings(ask, granted)
		if kind == "pairing" || kind == "review" {
			app.Capabilities = granted
		} else {
			app.Capabilities = normalizePermissionSet(unionStrings(app.Capabilities, granted))
		}
		app.Declared = normalizePermissionSet(unionStrings(app.Declared, granted))
	} else if kind == "pairing" {
		app.Capabilities = []string{}
	}
	app.Denied = normalizePermissionSet(subtractStrings(unionStrings(app.Denied, refused), granted))
	app.DontAsk = normalizePermissionSet(subtractStrings(app.DontAsk, granted))
	if dontAsk {
		app.DontAsk = normalizePermissionSet(unionStrings(app.DontAsk, refused))
	}
	if app.ApprovedAt == 0 {
		app.ApprovedAt = time.Now().UnixMilli()
	}
	return app
}

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

// permissionDemand is one in-flight prompt for a group of one app, shared by
// every call that waits for it.
type permissionDemand struct {
	done    chan struct{}
	granted []string
}

type permissionDemands struct {
	mu       sync.Mutex
	inflight map[string]*permissionDemand
	// asked remembers groups this agent run already prompted for after a
	// user action, so a retrying app cannot keep raising the prompt. The
	// "Permissions needed" chip asks regardless.
	asked map[string]bool
}

func (s *Server) demands() *permissionDemands {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permissionDemandState == nil {
		s.permissionDemandState = &permissionDemands{inflight: map[string]*permissionDemand{}, asked: map[string]bool{}}
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

// appIdentity finds the identity that holds this app's grant for a browser
// key. A browser reaching this machine through Dyner (relay, WebRTC) is
// known only by the pairing Dyner synced, which names no app; this app's
// grant then starts from that pairing, and a decision stores it under the
// app (see applyBrowserDecision).
func (s *Server) appIdentity(keyID, origin, storeID string) (BrowserIdentity, bool) {
	if identity, ok := s.findIdentity(keyID, origin, storeID); ok || storeID == "" {
		return identity, ok
	}
	synced, ok := s.findIdentity(keyID, origin, "")
	if !ok || synced.Authority || synced.Shared {
		return BrowserIdentity{}, false
	}
	synced.StoreID = storeID
	synced.ApprovedAt = 0
	return synced, true
}

// permissionRequiredError is a refusal the runtime can act on: Askable means
// the app declares the permission and the user has not said "Don't ask
// again", so the "Permissions needed" chip can offer it.
type permissionRequiredError struct {
	Permission string
	Askable    bool
	text       string
}

func (e *permissionRequiredError) Error() string { return e.text }

// fields are added to the refusal frame next to "error".
func (e *permissionRequiredError) fields() map[string]any {
	return map[string]any{"code": "permission-required", "permission": e.Permission, "askable": e.Askable}
}

// refusal builds the structured refusal for a permission auth lacks.
func (s *Server) refusal(auth *socketAuthentication, required, text string) *permissionRequiredError {
	if text == "" {
		text = permissionDeniedError(required)
	}
	return &permissionRequiredError{Permission: required, Askable: s.permissionAskable(auth, required), text: text}
}

// permissionAskable reports whether the user can still be asked for required.
func (s *Server) permissionAskable(auth *socketAuthentication, required string) bool {
	if auth == nil || auth.scoped || auth.storeID == "" || required == permissionsManage || !stringSliceHas(auth.declared, required) {
		return false
	}
	if auth.native {
		app, ok := s.nativeApp(auth.storeID)
		return ok && !stringSliceHas(app.DontAsk, required)
	}
	if auth.keyID == "" {
		return false
	}
	identity, ok := s.appIdentity(auth.keyID, auth.origin, auth.storeID)
	return ok && !identity.Shared && !stringSliceHas(identity.DontAsk, required)
}

// ensureCapability is the one place a refused call can turn into a prompt.
// It returns true when auth holds required, possibly after asking.
//
// A grant made meanwhile (another window, Dyner, Permissions…) is picked up
// silently. A native app asks in its own window only when the call follows a
// user action (the runtime marks those frames), once per group per agent run;
// the call then continues with the answer. Everything else is refused with a
// structured refusal that the runtime's "Permissions needed" chip offers.
func (s *Server) ensureCapability(ctx context.Context, auth *socketAuthentication, required string, userAction bool) bool {
	if auth == nil || required == "" || socketAllows(auth.capabilities, required) {
		return true
	}
	if !s.permissionAskable(auth, required) && !(auth != nil && auth.native) {
		return false
	}
	if auth.native {
		if app, ok := s.nativeApp(auth.storeID); ok && socketAllows(intersectStrings(app.Capabilities, auth.declared), required) {
			s.widenSocketGrant(auth, app.Capabilities)
			return true
		}
		if !userAction || !s.permissionAskable(auth, required) {
			return false
		}
		granted, _ := s.promptNativeGroup(ctx, auth, []string{required}, true)
		return socketAllows(granted, required) && socketAllows(auth.capabilities, required)
	}
	if identity, ok := s.appIdentity(auth.keyID, auth.origin, auth.storeID); ok && socketAllows(intersectStrings(identity.Capabilities, auth.declared), required) {
		s.widenSocketGrant(auth, identity.Capabilities)
		return true
	}
	return false
}

// requireCapability is ensureCapability for handlers that only hold the
// request, such as http.download needing fs.writeBase64. It returns nil or
// the structured refusal.
func (s *Server) requireCapability(ctx context.Context, request message, required, text string) error {
	if request.auth == nil || s.ensureCapability(ctx, request.auth, required, request.UserAction) {
		return nil
	}
	return s.refusal(request.auth, required, text)
}

// promptNativeGroup asks a native app's user about the declared, ungranted
// permissions in the groups of wanted, in the app's window, and returns what
// was granted. onceThisRun limits automatic prompts to one per group per
// agent run; the chip passes false.
func (s *Server) promptNativeGroup(ctx context.Context, auth *socketAuthentication, wanted []string, onceThisRun bool) ([]string, error) {
	app, ok := s.nativeApp(auth.storeID)
	if !ok {
		return nil, errors.New("this app is no longer installed")
	}
	groups := map[string]bool{}
	for _, permission := range wanted {
		groups[permissionGroupOf(permission)] = true
	}
	ask := []string{}
	for _, permission := range auth.declared {
		if permission == permissionsManage || socketAllows(app.Capabilities, permission) {
			continue
		}
		if stringSliceHas(wanted, permission) || groups[permissionGroupOf(permission)] {
			ask = append(ask, permission)
		}
	}
	ask = normalizePermissionSet(ask)
	if len(ask) == 0 {
		s.widenSocketGrant(auth, app.Capabilities)
		return intersectStrings(app.Capabilities, wanted), nil
	}
	focus := []string{}
	for group := range groups {
		focus = append(focus, group)
	}
	key := auth.storeID + "\x00" + strings.Join(normalizePermissionSet(focus), ",")
	state := s.demands()
	state.mu.Lock()
	if onceThisRun && state.asked[key] {
		state.mu.Unlock()
		return nil, nil
	}
	demand := state.inflight[key]
	owner := demand == nil
	if owner {
		demand = &permissionDemand{done: make(chan struct{})}
		state.inflight[key] = demand
		state.asked[key] = true
	}
	state.mu.Unlock()
	if owner {
		demand.granted = s.askNativeRequired(ctx, app, auth, ask, focus)
		state.mu.Lock()
		delete(state.inflight, key)
		state.mu.Unlock()
		close(demand.done)
	} else {
		select {
		case <-demand.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if updated, ok := s.nativeApp(auth.storeID); ok {
		s.widenSocketGrant(auth, updated.Capabilities)
	}
	return demand.granted, nil
}

// askNativeRequired shows the prompt in the app's window and stores the
// answer. It returns the newly granted permissions.
func (s *Server) askNativeRequired(ctx context.Context, app NativeApp, auth *socketAuthentication, ask, focus []string) []string {
	control := s.nativeControl(app.StoreID)
	if control == nil {
		return nil
	}
	name := auth.appName
	if name == "" {
		name = app.Name
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: "required", AppName: name, StoreID: app.StoreID, Origin: app.Origin, Declared: ask,
	})
	if err != nil {
		return nil
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
	if !send(control.socket, ctx, nativePermissionRequestFrame(pending.ID, "required", name, app.Origin, groups, focus)) {
		s.unregisterPending(pending.ID)
		return nil
	}
	waitCtx, stop := context.WithTimeout(ctx, requiredPermissionTimeout)
	defer stop()
	granted, _ := s.awaitDecision(waitCtx, pending)
	if !pending.wasResolved() {
		return nil
	}
	outcome, dontAsk := pending.Outcome()
	updated, ok := s.nativeApp(app.StoreID)
	if !ok {
		return nil
	}
	before := updated.Capabilities
	updated = applyNativeDecision(updated, "required", ask, granted, outcome, dontAsk)
	s.upsertNativeApp(updated)
	go s.notifyPermissionsChanged()
	added := subtractStrings(updated.Capabilities, before)
	if len(added) > 0 {
		go s.pushCapabilities(updated.StoreID, nativeKeyID(updated.StoreID), updated.Capabilities)
	}
	return added
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

// requestBrowserPermissions registers one pending Dyner request for the
// declared, ungranted permissions in the groups of wanted. The runtime opens
// the review page from the "Permissions needed" chip; the decision is applied
// to the identity and pushed to the app's pages.
func (s *Server) requestBrowserPermissions(auth *socketAuthentication, wanted []string) (*pendingRequest, error) {
	identity, ok := s.appIdentity(auth.keyID, auth.origin, auth.storeID)
	if !ok || identity.Shared {
		return nil, errors.New("this app has no permission grant on this computer")
	}
	groups := map[string]bool{}
	for _, permission := range wanted {
		groups[permissionGroupOf(permission)] = true
	}
	ask := []string{}
	for _, permission := range auth.declared {
		if permission == permissionsManage || socketAllows(identity.Capabilities, permission) {
			continue
		}
		if stringSliceHas(wanted, permission) || groups[permissionGroupOf(permission)] {
			ask = append(ask, permission)
		}
	}
	ask = normalizePermissionSet(ask)
	if len(ask) == 0 {
		return nil, nil
	}
	state := s.permissions()
	state.mu.Lock()
	for _, existing := range state.pending {
		if existing.Kind == "required" && existing.StoreID == auth.storeID && existing.Origin == auth.origin && sameCapabilitySet(existing.Declared, ask) && existing.ExpiresAt.After(time.Now()) {
			state.mu.Unlock()
			return existing, nil
		}
	}
	state.mu.Unlock()
	pending, err := s.registerPending(&pendingRequest{
		Kind: "required", AppName: auth.appName, StoreID: auth.storeID, Origin: auth.origin,
		Development: auth.development, Declared: ask,
	})
	if err != nil {
		return nil, err
	}
	keyID, storeID, origin := auth.keyID, auth.storeID, auth.origin
	go func() {
		granted, approved := s.awaitDecision(context.Background(), pending)
		if !pending.wasResolved() {
			return
		}
		_, dontAsk := pending.Outcome()
		s.applyBrowserDecision(keyID, origin, storeID, ask, granted, approved, dontAsk)
	}()
	return pending, nil
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

// applyBrowserDecision stores a Dyner decision about ask on a browser
// identity and pushes the new grant to the app's pages. Unchecked permissions
// count as refused; a grant clears a refusal and "Don't ask again". dontAsk
// (with a refusal) stops the app from raising the request again.
func (s *Server) applyBrowserDecision(keyID, origin, storeID string, ask, granted []string, approved, dontAsk bool) {
	identity, ok := s.appIdentity(keyID, origin, storeID)
	if !ok {
		return
	}
	granted = normalizePermissionSet(intersectStrings(granted, ask))
	refused := subtractStrings(ask, granted)
	identity.Capabilities = normalizePermissionSet(unionStrings(identity.Capabilities, granted))
	identity.Declared = normalizePermissionSet(unionStrings(identity.Declared, granted))
	identity.Denied = normalizePermissionSet(subtractStrings(unionStrings(identity.Denied, refused), granted))
	if dontAsk && !approved {
		identity.DontAsk = unionStrings(identity.DontAsk, ask)
	}
	identity.DontAsk = normalizePermissionSet(subtractStrings(identity.DontAsk, granted))
	if identity.ApprovedAt == 0 {
		// A decision about a Dyner-synced pairing is kept as this app's own
		// approval, which the next identity sync leaves in place.
		identity.ApprovedAt = time.Now().UnixMilli()
	}
	s.upsertBrowserIdentity(identity)
	go s.notifyPermissionsChanged()
	if approved && len(granted) > 0 {
		s.pushCapabilities(storeID, keyID, identity.Capabilities)
	}
}

// startBrowserDelta asks Dyner about permissions an app update added since
// this browser identity was approved. The app keeps running meanwhile.
func (s *Server) startBrowserDelta(identity BrowserIdentity, resolved resolvedApp, fresh []string, config Config) {
	fresh = normalizePermissionSet(subtractStrings(fresh, unionStrings([]string{permissionsManage}, unionStrings(identity.Denied, identity.DontAsk))))
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
		if pending.wasResolved() {
			_, dontAsk := pending.Outcome()
			s.applyBrowserDecision(identity.KeyID, identity.Origin, identity.StoreID, fresh, granted, approved, dontAsk)
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
	granted, outcome := hostDecision(frame)
	pending.resolveWith(intersectStrings(granted, pending.Declared), outcome, frame.DontAsk)
	return true
}

// handlePermissionRequestRPC implements `permissionRequest.request`: the
// runtime's "Permissions needed" chip (or the app) asks for declared
// permissions it lacks. A native app's user answers in the app's window; a
// browser app gets a pending Dyner request to open. Permissions the user
// marked "Don't ask again" are not asked.
func (s *Server) handlePermissionRequestRPC(ctx context.Context, request message) (any, error) {
	auth := request.auth
	if auth == nil || auth.scoped || auth.storeID == "" {
		return nil, errors.New("permission requests need an app connection")
	}
	if request.Method != "request" {
		return nil, errors.New("unsupported permissionRequest method")
	}
	wanted := []string{}
	for _, permission := range stringList(objectArg(request.Args, 0)["permissions"]) {
		if !socketAllows(auth.capabilities, permission) && s.permissionAskable(auth, permission) {
			wanted = append(wanted, permission)
		}
	}
	wanted = normalizePermissionSet(wanted)
	result := func() map[string]any {
		return map[string]any{"capabilities": auth.capabilities, "granted": len(wanted) > 0 && allAllowed(auth.capabilities, wanted)}
	}
	if len(wanted) == 0 {
		return result(), nil
	}
	if auth.native {
		if _, err := s.promptNativeGroup(ctx, auth, wanted, false); err != nil {
			return nil, err
		}
		return result(), nil
	}
	pending, err := s.requestBrowserPermissions(auth, wanted)
	if err != nil {
		return nil, err
	}
	out := result()
	if pending != nil {
		s.mu.Lock()
		config := s.Config
		s.mu.Unlock()
		out["pending"] = map[string]any{
			"requestId": pending.ID, "reviewUrl": config.reviewURL(pending.ID),
			"expiresInMs": time.Until(pending.ExpiresAt).Milliseconds(),
		}
	}
	return out, nil
}

func allAllowed(capabilities, wanted []string) bool {
	for _, permission := range wanted {
		if !socketAllows(capabilities, permission) {
			return false
		}
	}
	return true
}
