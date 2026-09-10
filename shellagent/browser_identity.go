package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"sort"
	"time"

	"nhooyr.io/websocket"
)

const directLocalAuthContext = "dynapp-direct-local-v1"

// Close codes shared with the PWA runtime.
const (
	closeIdentityRejected websocket.StatusCode = 4401
	closePairingDeclined  websocket.StatusCode = 4403
	closeApprovalRequired websocket.StatusCode = 4404
)

func BrowserKeyID(key BrowserJWK) string {
	sum := sha256.Sum256([]byte(key.X + "." + key.Y))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func directLocalPayload(nonce, origin, keyID string) []byte {
	return []byte(directLocalAuthContext + "\n" + nonce + "\n" + origin + "\n" + keyID)
}

type directLocalOutcome int

const (
	directLocalOK directLocalOutcome = iota
	directLocalProbe
	directLocalAborted
	directLocalRejected
	directLocalDeclined
	directLocalApprovalRequired
)

type directLocalProof struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	KeyID     string `json:"keyId"`
	Origin    string `json:"origin"`
	Signature string `json:"signature"`
}

// authenticateDirectLocal runs the browser-key challenge shared by the
// loopback WebSocket and WebTransport carriers. Unknown app identities wait
// for the Dyner client's decision; an unknown key from the authority origin
// is recorded as the authority itself.
func (s *Server) authenticateDirectLocal(ctx context.Context, socket protocolSocket, requestOrigin string) (socketAuthentication, directLocalOutcome) {
	// QUIC does not deliver a client-created stream to AcceptStream until the
	// client writes. The harmless init record breaks that transport-level
	// deadlock; it carries no credential or authority.
	_, initial, err := socket.Read(ctx)
	if err != nil {
		return socketAuthentication{}, directLocalAborted
	}
	var init directLocalInit
	if json.Unmarshal(initial, &init) != nil || (init.Version != 1 && init.Version != 2) {
		return socketAuthentication{}, directLocalRejected
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return socketAuthentication{}, directLocalAborted
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	if err := socket.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
		"type": "dynapp-direct-local-challenge", "version": 1, "nonce": nonce,
	})); err != nil {
		return socketAuthentication{}, directLocalAborted
	}
	if init.Type == "dynapp-direct-local-probe" {
		return socketAuthentication{}, directLocalProbe
	}
	if init.Type != "dynapp-direct-local-init" {
		return socketAuthentication{}, directLocalRejected
	}
	_, wire, err := socket.Read(ctx)
	if err != nil {
		return socketAuthentication{}, directLocalAborted
	}
	var proof directLocalProof
	if json.Unmarshal(wire, &proof) != nil || proof.Type != "dynapp-direct-local-auth" || proof.Version != 1 {
		return socketAuthentication{}, directLocalRejected
	}
	if _, ok := parseOrigin(proof.Origin); !ok || !originEquivalent(proof.Origin, requestOrigin) && proof.Origin != requestOrigin {
		return socketAuthentication{}, directLocalRejected
	}
	s.mu.Lock()
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	config := s.Config
	s.mu.Unlock()
	if identity, ok := matchDirectLocalIdentity(identities, init, proof, nonce, requestOrigin); ok {
		return s.continuePairedIdentity(ctx, socket, config, identity, init, requestOrigin)
	}
	if fresh, err := SyncBrowserIdentities(ctx, nil, config); err == nil {
		s.mu.Lock()
		s.Config.BrowserIdentities = mergeSyncedIdentities(s.Config.BrowserIdentities, fresh)
		merged := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
		s.mu.Unlock()
		if identity, ok := matchDirectLocalIdentity(merged, init, proof, nonce, requestOrigin); ok {
			return s.continuePairedIdentity(ctx, socket, config, identity, init, requestOrigin)
		}
	}
	return s.pairUnknownIdentity(ctx, socket, config, init, proof, nonce, requestOrigin)
}

// continuePairedIdentity builds the socket authentication for a known
// identity. A fresh Dyner pre-approval replaces the saved app grant on every
// catalog launch. Direct reconnects keep the saved grant and never open a
// permission UI from inside the app.
func (s *Server) continuePairedIdentity(ctx context.Context, socket protocolSocket, config Config, identity BrowserIdentity, init directLocalInit, requestOrigin string) (socketAuthentication, directLocalOutcome) {
	changed := applyBrowserClient(&identity, init.Client)
	persist := func() {
		if !changed {
			return
		}
		s.upsertBrowserIdentity(identity)
		go s.notifyPermissionsChanged()
	}
	if identity.Authority {
		if resolved, err := s.resolveManifestApp(ctx, config, config.catalogAppID()); err == nil {
			declared := unionStrings(resolved.Declared, []string{permissionsManage})
			if len(subtractStrings(declared, identity.Capabilities)) > 0 {
				identity.Capabilities = unionStrings(identity.Capabilities, declared)
				identity.Declared = unionStrings(identity.Declared, declared)
				identity.Connect = resolved.Connect
				changed = true
			}
		}
		persist()
		return authForIdentity(config, identity, nil), directLocalOK
	}
	var app *resolvedApp
	if init.Version >= 2 && init.App != nil {
		if resolved, err := s.resolveApp(ctx, requestOrigin, init); err == nil {
			app = &resolved
			if approval, ok := s.consumePreApproval(resolved.StoreID); ok {
				identity.Capabilities = normalizePermissionSet(intersectStrings(approval.Capabilities, resolved.Declared))
				identity.Declared = normalizePermissionSet(resolved.Declared)
				identity.Connect = resolved.Connect
				identity.ApprovedAt = time.Now().UnixMilli()
				if identity.StoreID == "" {
					identity.StoreID = resolved.StoreID
				}
				changed = true
				persist()
				return authForIdentity(config, identity, &resolved), directLocalOK
			} else if expanded := normalizePermissionSet(unionStrings(identity.Declared, resolved.Declared)); len(subtractStrings(expanded, identity.Declared)) > 0 {
				// Keep the existing grant. Raise the reviewed ceiling so Dyner
				// → Permissions can offer newly declared capabilities.
				identity.Declared = expanded
				if resolved.Connect != nil {
					identity.Connect = resolved.Connect
				}
				changed = true
				persist()
				return authForIdentity(config, identity, &resolved), directLocalOK
			}
		}
	}
	persist()
	return authForIdentity(config, identity, app), directLocalOK
}

func (s *Server) pairUnknownIdentity(ctx context.Context, socket protocolSocket, config Config, init directLocalInit, proof directLocalProof, nonce, requestOrigin string) (socketAuthentication, directLocalOutcome) {
	if init.Version < 2 || init.PublicKeyJWK == nil {
		return socketAuthentication{}, directLocalRejected
	}
	identity := BrowserIdentity{KeyID: proof.KeyID, PublicKeyJWK: *init.PublicKeyJWK, Origin: proof.Origin}
	applyBrowserClient(&identity, init.Client)
	if identity.Validate() != nil || !verifyBrowserProof(identity, nonce, proof.Origin, proof.Signature) {
		return socketAuthentication{}, directLocalRejected
	}
	if config.isAuthorityOrigin(requestOrigin) {
		return s.recordAuthorityIdentity(ctx, config, identity, init), directLocalOK
	}
	if init.App == nil {
		return socketAuthentication{}, directLocalRejected
	}
	resolved, err := s.resolveApp(ctx, requestOrigin, init)
	if err != nil {
		return socketAuthentication{}, directLocalRejected
	}
	// Only the authority identity may hold permissions.manage.
	resolved.Declared = subtractStrings(resolved.Declared, []string{permissionsManage})
	if approval, ok := s.consumePreApproval(resolved.StoreID); ok {
		// The Dyner catalog decided before launching the app: no pending reply.
		identity.StoreID = resolved.StoreID
		identity.Capabilities = normalizePermissionSet(intersectStrings(approval.Capabilities, resolved.Declared))
		identity.Declared = normalizePermissionSet(resolved.Declared)
		identity.Connect = resolved.Connect
		identity.ApprovedAt = time.Now().UnixMilli()
		s.upsertBrowserIdentity(identity)
		go s.notifyPermissionsChanged()
		return authForIdentity(config, identity, &resolved), directLocalOK
	}
	s.mu.Lock()
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	if existing, ok := findApprovedAppGrant(identities, proof.Origin, resolved.StoreID); ok {
		// The grant is for this hosted origin and store id. A new browser key
		// (PWA reinstall, another profile) reuses it instead of failing closed.
		identity.StoreID = resolved.StoreID
		identity.Capabilities = normalizePermissionSet(intersectStrings(existing.Capabilities, resolved.Declared))
		identity.Declared = normalizePermissionSet(unionStrings(existing.Declared, resolved.Declared))
		identity.Connect = resolved.Connect
		identity.ApprovedAt = time.Now().UnixMilli()
		s.upsertBrowserIdentity(identity)
		go s.notifyPermissionsChanged()
		return authForIdentity(config, identity, &resolved), directLocalOK
	}
	if s.autoApprovePairings {
		identity.StoreID = resolved.StoreID
		identity.Capabilities = normalizePermissionSet(resolved.Declared)
		identity.Declared = normalizePermissionSet(resolved.Declared)
		identity.Connect = resolved.Connect
		identity.ApprovedAt = time.Now().UnixMilli()
		s.upsertBrowserIdentity(identity)
		return authForIdentity(config, identity, &resolved), directLocalOK
	}
	// A direct launch on a fresh machine must remain usable. Keep the app's
	// authenticated connection open while Dyner presents the same permission
	// picker used by the catalog launch flow. The app cannot approve itself:
	// only the separately authenticated Dyner authority can decide this request.
	pending, err := s.registerPending(&pendingRequest{
		Kind: "pairing", AppName: resolved.Name, StoreID: resolved.StoreID, Origin: identity.Origin, Development: resolved.Development, Declared: resolved.Declared,
	})
	if err != nil {
		return socketAuthentication{}, directLocalRejected
	}
	if !s.sendPending(ctx, socket, config, pending) {
		s.unregisterPending(pending.ID)
		return socketAuthentication{}, directLocalAborted
	}
	granted, approved := s.awaitDecision(ctx, pending)
	if !approved {
		return socketAuthentication{}, directLocalDeclined
	}
	identity.StoreID = resolved.StoreID
	identity.Capabilities = normalizePermissionSet(intersectStrings(granted, resolved.Declared))
	identity.Declared = normalizePermissionSet(resolved.Declared)
	identity.Connect = resolved.Connect
	identity.ApprovedAt = time.Now().UnixMilli()
	s.upsertBrowserIdentity(identity)
	go s.notifyPermissionsChanged()
	return authForIdentity(config, identity, &resolved), directLocalOK
}

// recordAuthorityIdentity trusts the first key presented from the Dyner base
// origin (or a configured authority origin) without any decision: only the
// real Dyner client can present that Origin. It receives the catalog
// manifest's declared permissions plus permissions.manage.
func (s *Server) recordAuthorityIdentity(ctx context.Context, config Config, identity BrowserIdentity, init directLocalInit) socketAuthentication {
	catalogID := config.catalogAppID()
	var declared []string
	var connect map[string][]string
	if resolved, err := s.resolveManifestApp(ctx, config, catalogID); err == nil {
		declared, connect = resolved.Declared, resolved.Connect
	} else if init.App != nil {
		declared = normalizePermissionList(init.App.DeclaredPermissions)
		connect, _ = connectFromInit(init.App.Connect)
	}
	identity.Authority = true
	identity.StoreID = catalogID
	identity.Capabilities = unionStrings(declared, []string{permissionsManage})
	identity.Declared = append([]string(nil), identity.Capabilities...)
	identity.Connect = connect
	identity.ApprovedAt = time.Now().UnixMilli()
	s.upsertBrowserIdentity(identity)
	go s.notifyPermissionsChanged()
	return authForIdentity(config, identity, nil)
}

// authForIdentity computes the effective capability set: the authority's
// grant intersected with the current manifest's declared permissions.
func authForIdentity(config Config, identity BrowserIdentity, app *resolvedApp) socketAuthentication {
	declared := append([]string(nil), identity.Capabilities...)
	effective := append([]string(nil), identity.Capabilities...)
	connect := identity.Connect
	storeID := identity.StoreID
	appName := storeSlug(storeID)
	development := false
	if app != nil {
		declared = append([]string(nil), app.Declared...)
		effective = intersectStrings(identity.Capabilities, app.Declared)
		connect = app.Connect
		if storeID == "" {
			storeID = app.StoreID
		}
		appName = app.Name
		development = app.Development
	}
	effective = normalizePermissionSet(effective)
	environmentID := config.EnvironmentID
	if environmentID == "" {
		environmentID = "local"
	}
	return socketAuthentication{
		environment:  map[string]any{"id": environmentID, "name": "This device", "endpoint": nil, "state": "connected", "source": "local", "capabilities": effective},
		capabilities: effective,
		stored:       append([]string(nil), identity.Capabilities...),
		declared:     declared,
		connect:      connect,
		keyID:        identity.KeyID,
		origin:       identity.Origin,
		storeID:      storeID,
		appName:      appName,
		development:  development,
		ok:           true,
	}
}

func sameIdentityOrigin(left, right string) bool {
	return left == right || originEquivalent(left, right) || originEquivalent(right, left)
}

func findApprovedAppGrant(identities []BrowserIdentity, origin, storeID string) (BrowserIdentity, bool) {
	var best BrowserIdentity
	found := false
	bestPriority := 0
	for _, identity := range identities {
		if identity.Authority || !sameIdentityOrigin(identity.Origin, origin) {
			continue
		}
		priority := 0
		switch {
		case identity.StoreID == storeID && identity.ApprovedAt > 0:
			priority = 2
		case identity.StoreID == "" && len(identity.Capabilities) > 0:
			// Older agents stored one origin-bound grant without an app id or
			// approval timestamp. Reuse it once for a verified hosted app, then
			// pairUnknownIdentity writes the current app-scoped grant format.
			priority = 1
		default:
			continue
		}
		if !found || priority > bestPriority || priority == bestPriority && identity.ApprovedAt > best.ApprovedAt {
			best = identity
			found = true
			bestPriority = priority
		}
	}
	return best, found
}

func originEquivalent(identityOrigin, candidate string) bool {
	if identityOrigin == candidate {
		return true
	}
	identityURL, err := url.Parse(identityOrigin)
	if err != nil {
		return false
	}
	candidateURL, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	return identityURL.Host == candidateURL.Host && identityURL.Scheme == "https" && candidateURL.Scheme == "http" && privateOrLoopbackHost(candidateURL.Hostname())
}

func matchDirectLocalIdentity(identities []BrowserIdentity, init directLocalInit, proof directLocalProof, nonce, requestOrigin string) (BrowserIdentity, bool) {
	for _, identity := range identities {
		if identity.KeyID != proof.KeyID || !originEquivalent(identity.Origin, proof.Origin) || !originEquivalent(identity.Origin, requestOrigin) {
			continue
		}
		if identity.StoreID != "" && !identity.Authority && init.App != nil && identity.StoreID != init.App.StoreID {
			continue
		}
		if verifyBrowserProof(identity, nonce, proof.Origin, proof.Signature) {
			return identity, true
		}
	}
	return BrowserIdentity{}, false
}

func (s *Server) upsertBrowserIdentity(identity BrowserIdentity) {
	s.mu.Lock()
	replaced := false
	for index, existing := range s.Config.BrowserIdentities {
		if existing.KeyID == identity.KeyID && existing.Origin == identity.Origin && existing.StoreID == identity.StoreID {
			s.Config.BrowserIdentities[index] = identity
			replaced = true
		}
	}
	if !replaced {
		s.Config.BrowserIdentities = append(s.Config.BrowserIdentities, identity)
	}
	snapshot := s.Config
	stateDir := s.StateDir
	s.mu.Unlock()
	if stateDir != "" {
		_ = SaveConfig(stateDir, snapshot)
	}
}

// mergeSyncedIdentities keeps locally approved identities and replaces the
// Dyner-synced ones with the fresh list.
func mergeSyncedIdentities(existing, fresh []BrowserIdentity) []BrowserIdentity {
	merged := make([]BrowserIdentity, 0, len(existing)+len(fresh))
	for _, identity := range existing {
		if identity.ApprovedAt > 0 {
			merged = append(merged, identity)
		}
	}
	merged = append(merged, fresh...)
	normalized, err := normalizeBrowserIdentities(merged)
	if err != nil {
		return merged
	}
	return normalized
}

func (s *Server) hasBrowserIdentity(auth socketAuthentication) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, identity := range s.Config.BrowserIdentities {
		if identity.KeyID == auth.keyID && identity.Origin == auth.origin && (identity.StoreID == "" || identity.StoreID == auth.storeID) && sameCapabilitySet(identity.Capabilities, auth.stored) {
			return true
		}
	}
	return false
}

func verifyBrowserProof(identity BrowserIdentity, nonce, signedOrigin, encodedSignature string) bool {
	if identity.Origin != signedOrigin && !originEquivalent(identity.Origin, signedOrigin) {
		return false
	}
	x, err := base64.RawURLEncoding.DecodeString(identity.PublicKeyJWK.X)
	if err != nil || len(x) != 32 {
		return false
	}
	y, err := base64.RawURLEncoding.DecodeString(identity.PublicKeyJWK.Y)
	if err != nil || len(y) != 32 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != 64 {
		return false
	}
	public := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !public.Curve.IsOnCurve(public.X, public.Y) {
		return false
	}
	hash := sha256.Sum256(directLocalPayload(nonce, signedOrigin, identity.KeyID))
	return ecdsa.Verify(&public, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

func mustJSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }

func normalizeBrowserIdentities(value []BrowserIdentity) ([]BrowserIdentity, error) {
	seen := map[string]bool{}
	result := make([]BrowserIdentity, 0, len(value))
	for _, identity := range value {
		if err := identity.Validate(); err != nil {
			return nil, err
		}
		key := identity.KeyID + "\n" + identity.Origin + "\n" + identity.StoreID
		if seen[key] {
			continue
		}
		seen[key] = true
		identity.Capabilities = append([]string(nil), identity.Capabilities...)
		result = append(result, identity)
	}
	return result, nil
}

func subtractStrings(values, remove []string) []string {
	removed := map[string]bool{}
	for _, value := range remove {
		removed[value] = true
	}
	var result []string
	for _, value := range values {
		if !removed[value] {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func unionStrings(left, right []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range append(append([]string(nil), left...), right...) {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func intersectStrings(left, right []string) []string {
	allowed := map[string]bool{}
	for _, value := range right {
		allowed[value] = true
	}
	var result []string
	for _, value := range left {
		if allowed[value] {
			result = append(result, value)
		}
	}
	return result
}
