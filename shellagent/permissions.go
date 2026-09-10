package shellagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// pendingRequestTTL bounds how long a browser waits for Dyner's separately
// authenticated permission authority to decide a request.
const pendingRequestTTL = 10 * time.Minute

// permissionsManage is the capability the Dyner client needs to decide
// permissions for this agent. Only authority identities hold it.
const permissionsManage = "permissions.manage"

// permissionDeniedError is the exact text returned for a call whose
// permission the authority did not grant.
func permissionDeniedError(permission string) string {
	return fmt.Sprintf("Permission %s is not granted for this app. Review it in Dyner → Permissions.", permission)
}

// pendingRequest is one pairing or permission-delta decision waiting for the
// authority. Decisions arrive only through the `permissions` RPC.
type pendingRequest struct {
	ID          string
	Code        string
	Kind        string // "pairing" or "delta"
	StoreID     string
	AppName     string
	Origin      string
	Development bool
	// Declared lists the permissions the authority decides on: the manifest
	// set for a pairing, or only the new permissions for a delta.
	Declared  []string
	CreatedAt time.Time
	ExpiresAt time.Time
	decision  chan []string // nil slice means denied
	once      sync.Once
}

func (p *pendingRequest) resolve(granted []string, approved bool) {
	p.once.Do(func() {
		if !approved {
			p.decision <- nil
			return
		}
		if granted == nil {
			granted = []string{}
		}
		p.decision <- granted
	})
}

// preApproval is a decision the Dyner catalog made before launching an app,
// keyed by store id because the app page has no browser key yet.
type preApproval struct {
	StoreID      string
	Capabilities []string
	ExpiresAt    time.Time
}

const (
	defaultPreApprovalTTL = 10 * time.Minute
	maxPreApprovalTTL     = time.Hour
)

type permissionsState struct {
	mu          sync.Mutex
	pending     map[string]*pendingRequest
	preapproved map[string]preApproval
	subscribers map[protocolSocket]bool
	live        map[protocolSocket]*socketAuthentication
}

func (s *Server) permissions() *permissionsState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permissionState == nil {
		s.permissionState = &permissionsState{pending: map[string]*pendingRequest{}, preapproved: map[string]preApproval{}, subscribers: map[protocolSocket]bool{}, live: map[protocolSocket]*socketAuthentication{}}
	}
	return s.permissionState
}

func approvalCode() (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", value.Int64()), nil
}

func requestID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "req_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// registerPending adds a decision to the in-memory registry with a fresh id
// and six-digit code. The test carrier can auto-approve the declared set.
func (s *Server) registerPending(request *pendingRequest) (*pendingRequest, error) {
	state := s.permissions()
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	for id, pending := range state.pending {
		if pending.ExpiresAt.Before(now) {
			delete(state.pending, id)
		}
	}
	if len(state.pending) >= 32 {
		return nil, errors.New("too many pending permission requests")
	}
	for attempt := 0; attempt < 16; attempt++ {
		code, err := approvalCode()
		if err != nil {
			return nil, err
		}
		id, err := requestID()
		if err != nil {
			return nil, err
		}
		taken := false
		for _, pending := range state.pending {
			if pending.Code == code || pending.ID == id {
				taken = true
			}
		}
		if taken {
			continue
		}
		request.ID, request.Code = id, code
		request.CreatedAt, request.ExpiresAt = now, now.Add(pendingRequestTTL)
		request.Declared = normalizePermissionSet(request.Declared)
		request.decision = make(chan []string, 1)
		pending := request
		state.pending[id] = pending
		if s.autoApprovePairings {
			pending.resolve(append([]string(nil), pending.Declared...), true)
		}
		go s.notifyPermissionsChanged()
		return pending, nil
	}
	return nil, errors.New("could not allocate a permission request code")
}

func (s *Server) unregisterPending(id string) {
	state := s.permissions()
	state.mu.Lock()
	_, existed := state.pending[id]
	delete(state.pending, id)
	state.mu.Unlock()
	if existed {
		go s.notifyPermissionsChanged()
	}
}

// awaitDecision blocks until the authority decides, the request expires, or
// the socket goes away. It returns the granted set and whether it was
// approved.
func (s *Server) awaitDecision(ctx context.Context, pending *pendingRequest) ([]string, bool) {
	defer s.unregisterPending(pending.ID)
	timer := time.NewTimer(time.Until(pending.ExpiresAt))
	defer timer.Stop()
	select {
	case granted := <-pending.decision:
		return granted, granted != nil
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

func (s *Server) sendPending(ctx context.Context, socket protocolSocket, config Config, pending *pendingRequest) bool {
	return socket.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
		"type": "dynapp-direct-local-pending", "version": 3,
		"requestId": pending.ID, "code": pending.Code,
		"expiresInMs": pendingRequestTTL.Milliseconds(),
		"reviewUrl":   config.reviewURL(pending.ID),
	})) == nil
}

// trackLiveSocket remembers which identity an authenticated socket is bound
// to so `revoke` and `update` can close it.
func (s *Server) trackLiveSocket(socket protocolSocket, auth *socketAuthentication) func() {
	state := s.permissions()
	state.mu.Lock()
	state.live[socket] = auth
	state.mu.Unlock()
	return func() {
		state.mu.Lock()
		delete(state.live, socket)
		delete(state.subscribers, socket)
		state.mu.Unlock()
	}
}

func (s *Server) closeSocketsBoundTo(identity BrowserIdentity, code websocket.StatusCode, reason string) {
	state := s.permissions()
	state.mu.Lock()
	var targets []protocolSocket
	for socket, auth := range state.live {
		if auth.keyID == identity.KeyID && auth.origin == identity.Origin && (identity.StoreID == "" || auth.storeID == identity.StoreID) {
			targets = append(targets, socket)
		}
	}
	state.mu.Unlock()
	for _, socket := range targets {
		_ = socket.Close(code, reason)
	}
}

// notifyPermissionsChanged sends the `permissions` change event to every
// subscribed authority socket.
func (s *Server) notifyPermissionsChanged() {
	state := s.permissions()
	state.mu.Lock()
	targets := make([]protocolSocket, 0, len(state.subscribers))
	for socket := range state.subscribers {
		targets = append(targets, socket)
	}
	state.mu.Unlock()
	for _, socket := range targets {
		send(socket, context.Background(), map[string]any{"type": "rpc-event", "service": "permissions", "event": "changed"})
	}
}

// handlePermissionsRPC implements the `permissions` service used by the Dyner
// client. The frame loop has already required `permissions.manage`.
func (s *Server) handlePermissionsRPC(socket protocolSocket, request message) (any, error) {
	if request.auth == nil || !request.auth.allows(permissionsManage) {
		return nil, errors.New(permissionDeniedError(permissionsManage))
	}
	switch request.Method {
	case "list":
		return s.listPermissions(), nil
	case "decide":
		return s.decidePermissions(objectArg(request.Args, 0))
	case "update":
		return s.updateGrant(objectArg(request.Args, 0))
	case "revoke":
		return s.revokeGrant(objectArg(request.Args, 0))
	case "preapprove":
		return s.preapprovePermissions(objectArg(request.Args, 0))
	case "subscribe":
		state := s.permissions()
		state.mu.Lock()
		state.subscribers[socket] = true
		state.mu.Unlock()
		return map[string]any{"ok": true, "subscribed": true}, nil
	default:
		return nil, errors.New("unsupported permissions method")
	}
}

func (s *Server) listPermissions() map[string]any {
	state := s.permissions()
	state.mu.Lock()
	now := time.Now()
	requests := make([]*pendingRequest, 0, len(state.pending))
	for _, pending := range state.pending {
		if pending.ExpiresAt.After(now) {
			requests = append(requests, pending)
		}
	}
	state.mu.Unlock()
	sort.Slice(requests, func(i, j int) bool { return requests[i].CreatedAt.Before(requests[j].CreatedAt) })
	pending := make([]map[string]any, 0, len(requests))
	for _, item := range requests {
		pending = append(pending, map[string]any{
			"id": item.ID, "code": item.Code, "kind": item.Kind, "storeId": item.StoreID, "appName": item.AppName,
			"origin": item.Origin, "development": item.Development,
			"declared":  describePermissions(item.Declared),
			"suggested": suggestedPermissions(item.Declared),
			"expiresAt": item.ExpiresAt.UnixMilli(),
		})
	}
	preapproved := make([]map[string]any, 0)
	for _, approval := range s.livePreApprovals() {
		preapproved = append(preapproved, map[string]any{"storeId": approval.StoreID, "capabilities": approval.Capabilities, "expiresAt": approval.ExpiresAt.UnixMilli()})
	}
	s.mu.Lock()
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	grants := make([]map[string]any, 0, len(identities))
	for _, identity := range identities {
		grants = append(grants, grantPayload(identity))
	}
	return map[string]any{"pending": pending, "grants": grants, "preapproved": preapproved}
}

// livePreApprovals returns unexpired pre-approvals sorted by store id and
// drops expired ones from the registry.
func (s *Server) livePreApprovals() []preApproval {
	state := s.permissions()
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	result := make([]preApproval, 0, len(state.preapproved))
	for storeID, approval := range state.preapproved {
		if !approval.ExpiresAt.After(now) {
			delete(state.preapproved, storeID)
			continue
		}
		result = append(result, approval)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StoreID < result[j].StoreID })
	return result
}

// preapprovePermissions records a decision made before the app launches. A
// newer call for the same store id replaces the older one.
func (s *Server) preapprovePermissions(args map[string]any) (any, error) {
	storeID := strings.TrimSpace(stringValue(args["storeId"]))
	if !validStoreID(storeID) {
		return nil, errors.New("a valid app store id is required")
	}
	capabilities, present := capabilityArg(args)
	if !present {
		return nil, errors.New("capabilities must be a list")
	}
	capabilities = subtractStrings(capabilities, []string{permissionsManage})
	if capabilities == nil {
		capabilities = []string{}
	}
	ttl := defaultPreApprovalTTL
	if value, ok := args["expiresInMs"].(float64); ok && value > 0 {
		ttl = time.Duration(value) * time.Millisecond
	}
	if ttl > maxPreApprovalTTL {
		ttl = maxPreApprovalTTL
	}
	approval := preApproval{StoreID: storeID, Capabilities: capabilities, ExpiresAt: time.Now().Add(ttl)}
	state := s.permissions()
	state.mu.Lock()
	state.preapproved[storeID] = approval
	state.mu.Unlock()
	go s.notifyPermissionsChanged()
	return map[string]any{"ok": true, "storeId": storeID, "capabilities": capabilities, "expiresAt": approval.ExpiresAt.UnixMilli()}, nil
}

// consumePreApproval returns and removes the live pre-approval for a store id.
func (s *Server) consumePreApproval(storeID string) (preApproval, bool) {
	state := s.permissions()
	state.mu.Lock()
	defer state.mu.Unlock()
	approval, ok := state.preapproved[storeID]
	if !ok {
		return preApproval{}, false
	}
	delete(state.preapproved, storeID)
	if !approval.ExpiresAt.After(time.Now()) {
		return preApproval{}, false
	}
	return approval, true
}

func grantPayload(identity BrowserIdentity) map[string]any {
	capabilities := identity.Capabilities
	if capabilities == nil {
		capabilities = []string{}
	}
	payload := map[string]any{
		"keyId": identity.KeyID, "origin": identity.Origin, "storeId": identity.StoreID, "appName": storeSlug(identity.StoreID),
		"capabilities": capabilities, "approvedAt": identity.ApprovedAt, "authority": identity.Authority,
		"declared": normalizePermissionSet(identity.Declared),
	}
	if !identity.Client.Empty() {
		payload["client"] = identity.Client
	}
	return payload
}

// capabilityArg reads `capabilities`; it distinguishes an explicit null (deny)
// from a list.
func capabilityArg(args map[string]any) ([]string, bool) {
	value, present := args["capabilities"]
	if !present || value == nil {
		return nil, false
	}
	return normalizePermissionSet(stringList(value)), true
}

func (s *Server) decidePermissions(args map[string]any) (any, error) {
	id := strings.TrimSpace(stringValue(args["id"]))
	if id == "" {
		return nil, errors.New("a pending request id is required")
	}
	state := s.permissions()
	state.mu.Lock()
	pending := state.pending[id]
	state.mu.Unlock()
	if pending == nil || pending.ExpiresAt.Before(time.Now()) {
		return nil, errors.New("no pending permission request has that id")
	}
	requested, approved := capabilityArg(args)
	granted := []string{}
	if approved {
		granted = intersectStrings(requested, pending.Declared)
		sort.Strings(granted)
	}
	pending.resolve(granted, approved)
	return map[string]any{"ok": true, "id": id, "approved": approved, "granted": granted}, nil
}

func grantSelector(args map[string]any) (keyID, origin, storeID string, err error) {
	keyID = strings.TrimSpace(stringValue(args["keyId"]))
	origin = strings.TrimSpace(stringValue(args["origin"]))
	storeID = strings.TrimSpace(stringValue(args["storeId"]))
	if keyID == "" || origin == "" {
		return "", "", "", errors.New("keyId and origin are required")
	}
	return keyID, origin, storeID, nil
}

func (s *Server) findIdentity(keyID, origin, storeID string) (BrowserIdentity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, identity := range s.Config.BrowserIdentities {
		if identity.KeyID == keyID && identity.Origin == origin && identity.StoreID == storeID {
			return identity, true
		}
	}
	return BrowserIdentity{}, false
}

func (s *Server) declaredCeiling(storeID string, stored []string) []string {
	ceiling := normalizePermissionSet(stored)
	if s == nil || !validStoreID(storeID) {
		return ceiling
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	config := s.Config
	s.mu.Unlock()
	resolved, err := s.resolveManifestApp(ctx, config, storeID)
	if err != nil {
		return ceiling
	}
	return normalizePermissionSet(unionStrings(ceiling, resolved.Declared))
}

func (s *Server) updateGrant(args map[string]any) (any, error) {
	keyID, origin, storeID, err := grantSelector(args)
	if err != nil {
		return nil, err
	}
	identity, ok := s.findIdentity(keyID, origin, storeID)
	if !ok {
		return nil, errors.New("no grant matches that identity")
	}
	requested, present := capabilityArg(args)
	if !present {
		return nil, errors.New("capabilities must be a list")
	}
	// Clamp to the reviewed manifest set when one is known. Union in the
	// current catalog declaration so a grant created before a new permission
	// existed can still receive it from Dyner → Permissions.
	ceiling := s.declaredCeiling(storeID, identity.Declared)
	if len(ceiling) > 0 {
		requested = intersectStrings(requested, ceiling)
	}
	if identity.Authority {
		requested = unionStrings(requested, []string{permissionsManage})
	} else {
		requested = subtractStrings(requested, []string{permissionsManage})
	}
	identity.Capabilities = normalizePermissionSet(requested)
	identity.Declared = unionStrings(ceiling, identity.Capabilities)
	if identity.ApprovedAt == 0 {
		identity.ApprovedAt = time.Now().UnixMilli()
	}
	s.upsertBrowserIdentity(identity)
	s.closeSocketsBoundTo(identity, websocket.StatusServiceRestart, "Permissions changed")
	go s.notifyPermissionsChanged()
	return map[string]any{"ok": true, "grant": grantPayload(identity)}, nil
}

func (s *Server) revokeGrant(args map[string]any) (any, error) {
	keyID, origin, storeID, err := grantSelector(args)
	if err != nil {
		return nil, err
	}
	identity, ok := s.findIdentity(keyID, origin, storeID)
	if !ok {
		return nil, errors.New("no grant matches that identity")
	}
	s.mu.Lock()
	kept := make([]BrowserIdentity, 0, len(s.Config.BrowserIdentities))
	for _, existing := range s.Config.BrowserIdentities {
		if existing.KeyID == keyID && existing.Origin == origin && existing.StoreID == storeID {
			continue
		}
		kept = append(kept, existing)
	}
	s.Config.BrowserIdentities = kept
	snapshot := s.Config
	stateDir := s.StateDir
	s.mu.Unlock()
	if stateDir != "" {
		_ = SaveConfig(stateDir, snapshot)
	}
	s.closeSocketsBoundTo(identity, closePairingDeclined, "Pairing revoked")
	go s.notifyPermissionsChanged()
	return map[string]any{"ok": true}, nil
}

func stringSliceHas(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
