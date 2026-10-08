package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

// nativeKeyPrefix marks grants that belong to a natively installed app rather
// than a browser key. Dyner's Permissions screen addresses them through the
// same keyId/origin/storeId selector as browser grants.
const nativeKeyPrefix = "native:"

// NativeApp is one app installed as a native desktop app (see
// docs/native-host.md in the dynapp repository). It carries both the install
// record and the app's capability grant.
type NativeApp struct {
	StoreID string `json:"storeId"`
	Name    string `json:"name"`
	Origin  string `json:"origin"`
	URL     string `json:"url"`
	// Path is the macOS app bundle or the Windows Start menu shortcut.
	Path string `json:"path"`
	// Executable and CDHash identify the macOS host process that may connect
	// for this app. Windows hosts run the agent executable itself.
	Executable  string `json:"executable,omitempty"`
	CDHash      string `json:"cdhash,omitempty"`
	BundleID    string `json:"bundleId,omitempty"`
	InstalledAt int64  `json:"installedAt"`
	// Capabilities is the granted set. Declared is every permission the user
	// has reviewed; permissions declared later are asked as a delta.
	Capabilities []string            `json:"capabilities"`
	Declared     []string            `json:"declared,omitempty"`
	Connect      map[string][]string `json:"connect,omitempty"`
	// ApprovedAt is zero until the user has made a decision.
	ApprovedAt int64 `json:"approvedAt,omitempty"`
	// Authority marks a native install of the Dyner catalog.
	Authority bool `json:"authority,omitempty"`
}

// NativeAppList decodes leniently: an entry that does not parse or validate
// is skipped, so native-app state can never stop an agent from loading its
// browser pairings and grants.
type NativeAppList []NativeApp

func (list *NativeAppList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		*list = nil
		return nil
	}
	apps := make(NativeAppList, 0, len(raw))
	for _, entry := range raw {
		var app NativeApp
		if json.Unmarshal(entry, &app) == nil && app.Validate() == nil {
			apps = append(apps, app)
		}
	}
	*list = apps
	return nil
}

// Validate checks the fields the native channel trusts.
func (app NativeApp) Validate() error {
	if !validStoreID(app.StoreID) {
		return errors.New("native app store id is invalid")
	}
	parsed, ok := parseOrigin(app.Origin)
	if !ok || parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHostname(parsed.Hostname())) {
		return errors.New("native app origin is invalid")
	}
	if strings.TrimSpace(app.Path) == "" {
		return errors.New("native app path is required")
	}
	return nil
}

func nativeKeyID(storeID string) string { return nativeKeyPrefix + storeID }

// nativeAvailable reports whether this agent may install and host native
// apps. Platforms still in preview stay dormant unless the user opts in, so
// an agent update cannot change their behavior.
func (s *Server) nativeAvailable() (bool, string) {
	if nativeRequiresOptIn() {
		s.mu.Lock()
		enabled := s.Config.NativeAppsEnabled
		s.mu.Unlock()
		if !enabled && !EnvEnabled("DYNAPP_NATIVE_APPS") {
			return false, "native apps on " + nativePlatformName() + " are in preview; set DYNAPP_NATIVE_APPS=1 or nativeAppsEnabled in the agent config to try them"
		}
	}
	return nativeSupported()
}

// nativeDocumentType is an app-declared document association.
type nativeDocumentType struct {
	Name       string   `json:"name"`
	Extensions []string `json:"extensions"`
	MimeTypes  []string `json:"mimeTypes"`
	Role       string   `json:"role"`
	Rank       string   `json:"rank"`
}

// nativeInstallSpec is what a platform needs to materialize one app.
type nativeInstallSpec struct {
	DocumentTypes []nativeDocumentType
	StoreID       string
	Name          string
	Origin        string
	URL           string
	Icon          []byte
	AgentEndpoint string
	StateDir      string
}

type nativeInstallResult struct {
	Path       string
	Executable string
	CDHash     string
	BundleID   string
}

func (s *Server) nativeApp(storeID string) (NativeApp, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, app := range s.Config.NativeApps {
		if app.StoreID == storeID {
			return app, true
		}
	}
	return NativeApp{}, false
}

func (s *Server) upsertNativeApp(app NativeApp) {
	if app.Validate() != nil {
		return
	}
	s.mu.Lock()
	replaced := false
	for index, existing := range s.Config.NativeApps {
		if existing.StoreID == app.StoreID {
			s.Config.NativeApps[index] = app
			replaced = true
		}
	}
	if !replaced {
		s.Config.NativeApps = append(s.Config.NativeApps, app)
	}
	snapshot := s.Config
	stateDir := s.StateDir
	s.mu.Unlock()
	if stateDir != "" {
		_ = SaveConfig(stateDir, snapshot)
	}
}

func (s *Server) removeNativeApp(storeID string) {
	s.mu.Lock()
	kept := make([]NativeApp, 0, len(s.Config.NativeApps))
	for _, existing := range s.Config.NativeApps {
		if existing.StoreID != storeID {
			kept = append(kept, existing)
		}
	}
	s.Config.NativeApps = kept
	snapshot := s.Config
	stateDir := s.StateDir
	s.mu.Unlock()
	if stateDir != "" {
		_ = SaveConfig(stateDir, snapshot)
	}
}

// closeNativeSockets closes every page connection of one native app so it
// reconnects with the current grant.
func (s *Server) closeNativeSockets(app NativeApp, code websocket.StatusCode, reason string) {
	s.closeSocketsBoundTo(BrowserIdentity{KeyID: nativeKeyID(app.StoreID), Origin: app.Origin, StoreID: app.StoreID}, code, reason)
}

func (s *Server) hasNativeGrant(auth socketAuthentication) bool {
	app, ok := s.nativeApp(strings.TrimPrefix(auth.keyID, nativeKeyPrefix))
	return ok && app.Origin == auth.origin && sameCapabilitySet(app.Capabilities, auth.stored)
}

// authForNative is authForIdentity for a native app: the stored grant
// intersected with the current manifest when Dyner could be reached.
func authForNative(config Config, app NativeApp, resolved *resolvedApp) socketAuthentication {
	declared := normalizePermissionSet(app.Declared)
	effective := intersectStrings(app.Capabilities, declared)
	connect := app.Connect
	name := app.Name
	if resolved != nil {
		declared = append([]string(nil), resolved.Declared...)
		if app.Authority {
			declared = unionStrings(declared, []string{permissionsManage})
		}
		effective = intersectStrings(app.Capabilities, declared)
		connect = resolved.Connect
		if resolved.Name != "" {
			name = resolved.Name
		}
	}
	if app.Authority {
		effective = unionStrings(effective, []string{permissionsManage})
	} else {
		effective = subtractStrings(effective, []string{permissionsManage})
	}
	effective = normalizePermissionSet(effective)
	environmentID := config.EnvironmentID
	if environmentID == "" {
		environmentID = "local"
	}
	return socketAuthentication{
		environment:  map[string]any{"id": environmentID, "name": "This device", "endpoint": nil, "state": "connected", "source": "local", "capabilities": effective},
		capabilities: effective,
		stored:       append([]string(nil), app.Capabilities...),
		declared:     declared,
		connect:      connect,
		keyID:        nativeKeyID(app.StoreID),
		origin:       app.Origin,
		storeID:      app.StoreID,
		appName:      name,
		ok:           true,
	}
}

func nativeGrantPayload(app NativeApp) map[string]any {
	capabilities := app.Capabilities
	if capabilities == nil {
		capabilities = []string{}
	}
	return map[string]any{
		"keyId": nativeKeyID(app.StoreID), "origin": app.Origin, "storeId": app.StoreID, "appName": app.Name,
		"capabilities": capabilities, "approvedAt": app.ApprovedAt, "authority": app.Authority,
		"declared": normalizePermissionSet(app.Declared),
		"client":   BrowserClient{Platform: runtime.GOOS, Mode: "native", Label: "Installed app"},
		"native":   true,
	}
}

// nativeManifest is a resolved app plus what the native prompt and installer
// need beyond resolvedApp.
type nativeManifest struct {
	app           resolvedApp
	origin        string
	reasons       map[string]string
	documentTypes []nativeDocumentType
}

func (s *Server) resolveNativeManifest(ctx context.Context, config Config, storeID string) (nativeManifest, error) {
	detail, err := s.fetchDynerAppDetail(ctx, config, storeID)
	if err != nil {
		return nativeManifest{}, err
	}
	resolved, err := appFromDynerManifest(storeID, detail)
	if err != nil {
		return nativeManifest{}, err
	}
	origin := ""
	app, _ := detail["app"].(map[string]any)
	hosted := stringList(detail["hostedOrigins"])
	if len(hosted) == 0 && app != nil {
		hosted = stringList(app["hostedOrigins"])
	}
	for _, candidate := range hosted {
		// HTTPS, or plain HTTP on loopback for a development Dyner.
		if parsed, ok := parseOrigin(strings.TrimRight(strings.TrimSpace(candidate), "/")); ok && (parsed.Scheme == "https" || parsed.Scheme == "http" && loopbackHostname(parsed.Hostname())) {
			origin = strings.ToLower(parsed.Scheme + "://" + parsed.Host)
			break
		}
	}
	if origin == "" {
		origin = legacyHostedOrigin(config, storeID)
	}
	if origin == "" {
		return nativeManifest{}, errors.New("Dyner did not report a hosted origin for this app")
	}
	return nativeManifest{app: resolved, origin: origin, reasons: manifestPermissionReasons(detail), documentTypes: manifestDocumentTypes(detail)}, nil
}

// legacyHostedOrigin is the app host Dyner uses when hostedOrigins is absent:
// `https://<owner-without-hyphens>-<slug>.<appDomain>`.
func legacyHostedOrigin(config Config, storeID string) string {
	if storeID == config.catalogAppID() {
		if hosted := config.catalogHostedOrigin(); hosted != "" {
			return hosted
		}
	}
	domain := config.appDomain()
	owner, slug, found := strings.Cut(storeID, "/")
	if domain == "" || !found {
		return ""
	}
	return "https://" + strings.ToLower(strings.ReplaceAll(owner, "-", "")+"-"+slug+"."+domain)
}

// manifestPermissionReasons maps each declared permission to the reason the
// app gives for it in app.json.
func manifestPermissionReasons(detail map[string]any) map[string]string {
	reasons := map[string]string{}
	revision, _ := detail["latestRevision"].(map[string]any)
	if revision == nil {
		if revisions, _ := detail["revisions"].([]any); len(revisions) > 0 {
			revision, _ = revisions[0].(map[string]any)
		}
	}
	manifest, _ := revision["manifest"].(map[string]any)
	entries, _ := manifest["backendPermissions"].([]any)
	for _, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		permission := strings.TrimSpace(stringValue(object["permission"]))
		reason := strings.TrimSpace(stringValue(object["reason"]))
		if permission != "" && reason != "" {
			reasons[permission] = truncateText(reason, 400)
		}
	}
	return reasons
}

// nativePermissionGroups builds the prompt content for the permissions being
// decided. granted marks what the app holds now; suggested is the default
// state for a new decision.
func nativePermissionGroups(ask, granted []string, reasons map[string]string) []map[string]any {
	suggested := map[string]bool{}
	for _, permission := range suggestedPermissions(ask) {
		suggested[permission] = true
	}
	holding := map[string]bool{}
	for _, permission := range granted {
		holding[permission] = true
	}
	groups := []map[string]any{}
	for _, group := range groupPermissions(ask) {
		groupReasons := []string{}
		seen := map[string]bool{}
		allGranted, anySuggested := true, false
		for _, permission := range group.Permissions {
			if reason := reasons[permission]; reason != "" && !seen[reason] {
				seen[reason] = true
				groupReasons = append(groupReasons, reason)
			}
			allGranted = allGranted && holding[permission]
			anySuggested = anySuggested || suggested[permission]
		}
		groups = append(groups, map[string]any{
			"id": group.ID, "title": group.Title, "summary": group.Summary, "danger": group.Danger,
			"permissions": group.Permissions, "reasons": groupReasons,
			"granted": allGranted, "suggested": anySuggested && group.Danger != "very-high",
		})
	}
	return groups
}

// handleNativeAppsRPC implements the native install methods of the `apps`
// service. Only the permission authority (the Dyner catalog) may call them.
func (s *Server) handleNativeAppsRPC(ctx context.Context, request message) (any, error) {
	if request.auth == nil || !request.auth.allows(permissionsManage) {
		return nil, errors.New(permissionDeniedError(permissionsManage))
	}
	args := objectArg(request.Args, 0)
	switch request.Method {
	case "nativeSupport":
		supported, reason := s.nativeAvailable()
		result := map[string]any{"supported": supported, "platform": nativePlatformName()}
		if reason != "" {
			result["reason"] = reason
		}
		return result, nil
	case "listNative":
		s.mu.Lock()
		apps := append([]NativeApp(nil), s.Config.NativeApps...)
		s.mu.Unlock()
		sort.Slice(apps, func(i, j int) bool { return apps[i].StoreID < apps[j].StoreID })
		list := make([]map[string]any, 0, len(apps))
		for _, app := range apps {
			list = append(list, map[string]any{
				"storeId": app.StoreID, "name": app.Name, "origin": app.Origin, "path": app.Path,
				"installedAt": app.InstalledAt, "capabilities": normalizePermissionSet(app.Capabilities),
			})
		}
		return map[string]any{"apps": list}, nil
	case "installNative":
		return s.installNativeApp(ctx, args)
	case "uninstallNative":
		return s.uninstallNativeApp(args)
	case "launchNative":
		app, ok := s.nativeApp(strings.TrimSpace(stringValue(args["storeId"])))
		if !ok {
			return nil, errors.New("this app is not installed as a native app")
		}
		if err := nativeLaunch(app, s.nativeEndpoint()); err != nil {
			return nil, err
		}
		return map[string]any{"launched": true}, nil
	default:
		return nil, errors.New("unsupported apps method")
	}
}

func (s *Server) installNativeApp(ctx context.Context, args map[string]any) (any, error) {
	if supported, reason := s.nativeAvailable(); !supported {
		return nil, fmt.Errorf("native apps are not available here: %s", reason)
	}
	storeID := strings.TrimSpace(stringValue(args["storeId"]))
	if !validStoreID(storeID) {
		return nil, errors.New("a valid app store id is required")
	}
	requested, present := capabilityArg(args)
	if !present {
		requested = []string{}
	}
	s.nativeInstallMu.Lock()
	defer s.nativeInstallMu.Unlock()
	s.mu.Lock()
	config := s.Config
	stateDir := s.StateDir
	s.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manifest, err := s.resolveNativeManifest(resolveCtx, config, storeID)
	if err != nil {
		return nil, err
	}
	client := s.dynerHTTPClient()
	startURL := fetchNativeStartURL(resolveCtx, client, manifest.origin)
	icon, err := fetchNativeIcon(resolveCtx, client, manifest.origin)
	if err != nil {
		return nil, err
	}
	result, err := nativeInstall(nativeInstallSpec{
		StoreID: storeID, Name: manifest.app.Name, Origin: manifest.origin, URL: startURL, Icon: icon,
		AgentEndpoint: s.nativeEndpoint(), StateDir: stateDir, DocumentTypes: manifest.documentTypes,
	})
	if err != nil {
		return nil, err
	}
	declared := subtractStrings(manifest.app.Declared, []string{permissionsManage})
	app := NativeApp{
		StoreID: storeID, Name: manifest.app.Name, Origin: manifest.origin, URL: startURL,
		Path: result.Path, Executable: result.Executable, CDHash: result.CDHash, BundleID: result.BundleID,
		InstalledAt: time.Now().UnixMilli(), Connect: manifest.app.Connect, ApprovedAt: time.Now().UnixMilli(),
		Capabilities: normalizePermissionSet(intersectStrings(requested, declared)),
		Declared:     normalizePermissionSet(declared),
	}
	if previous, ok := s.nativeApp(storeID); ok && previous.InstalledAt != 0 {
		app.InstalledAt = previous.InstalledAt
	}
	if storeID == config.catalogAppID() {
		app.Authority = true
		app.Capabilities = normalizePermissionSet(unionStrings(declared, []string{permissionsManage}))
		app.Declared = append([]string(nil), app.Capabilities...)
	}
	s.upsertNativeApp(app)
	s.closeNativeSockets(app, websocket.StatusServiceRestart, "Permissions changed")
	go s.notifyPermissionsChanged()
	launched := false
	if launch, _ := args["launch"].(bool); launch {
		if err := nativeLaunch(app, s.nativeEndpoint()); err != nil {
			return nil, fmt.Errorf("installed %s but could not open it: %w", app.Name, err)
		}
		launched = true
	}
	return map[string]any{"storeId": storeID, "name": app.Name, "path": app.Path, "launched": launched}, nil
}

func (s *Server) uninstallNativeApp(args map[string]any) (any, error) {
	storeID := strings.TrimSpace(stringValue(args["storeId"]))
	s.nativeInstallMu.Lock()
	defer s.nativeInstallMu.Unlock()
	app, ok := s.nativeApp(storeID)
	if !ok {
		return nil, errors.New("this app is not installed as a native app")
	}
	if err := nativeUninstall(app); err != nil {
		return nil, err
	}
	s.removeNativeApp(storeID)
	s.closeNativeSockets(app, closePairingDeclined, "App uninstalled")
	go s.notifyPermissionsChanged()
	return map[string]any{"removed": true}, nil
}

func (s *Server) updateNativeGrant(storeID string, requested []string) (any, error) {
	app, ok := s.nativeApp(storeID)
	if !ok {
		return nil, errors.New("no grant matches that identity")
	}
	ceiling := s.declaredCeiling(storeID, app.Declared)
	if len(ceiling) > 0 {
		requested = intersectStrings(requested, ceiling)
	}
	if app.Authority {
		requested = unionStrings(requested, []string{permissionsManage})
	} else {
		requested = subtractStrings(requested, []string{permissionsManage})
	}
	app.Capabilities = normalizePermissionSet(requested)
	app.Declared = unionStrings(ceiling, app.Capabilities)
	if app.ApprovedAt == 0 {
		app.ApprovedAt = time.Now().UnixMilli()
	}
	s.upsertNativeApp(app)
	s.closeNativeSockets(app, websocket.StatusServiceRestart, "Permissions changed")
	go s.notifyPermissionsChanged()
	return map[string]any{"ok": true, "grant": nativeGrantPayload(app)}, nil
}

// revokeNativeGrant clears the decision but keeps the install: the next
// launch asks again in the app's own window.
func (s *Server) revokeNativeGrant(storeID string) (any, error) {
	app, ok := s.nativeApp(storeID)
	if !ok {
		return nil, errors.New("no grant matches that identity")
	}
	app.Capabilities = []string{}
	app.Declared = nil
	app.ApprovedAt = 0
	if app.Authority {
		app.Capabilities = []string{permissionsManage}
	}
	s.upsertNativeApp(app)
	s.closeNativeSockets(app, closePairingDeclined, "Pairing revoked")
	go s.notifyPermissionsChanged()
	return map[string]any{"ok": true}, nil
}

const maxNativeIconBytes = 4 << 20

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// fetchNativeIcon prefers the full-bleed maskable image on macOS and Android,
// where the bundle builder or launcher applies the platform mask without
// shrinking the glyph.
func fetchNativeIcon(ctx context.Context, client *http.Client, origin string) ([]byte, error) {
	iconURL := origin + "/icon-512.png"
	maskable := runtime.GOOS == "darwin" || runtime.GOOS == "android"
	if maskable {
		iconURL = origin + "/icon-512-maskable.png"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("could not download the app icon: %w", err)
	}
	if maskable && response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, origin+"/icon-512.png", nil)
		if err != nil {
			return nil, err
		}
		response, err = client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("could not download the app icon: %w", err)
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the app icon returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxNativeIconBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxNativeIconBytes || !bytes.HasPrefix(data, pngSignature) {
		return nil, errors.New("the app icon is not a PNG image")
	}
	return data, nil
}

// fetchNativeStartURL reads start_url from the app's web manifest and keeps
// it only when it stays on the app origin.
func fetchNativeStartURL(ctx context.Context, client *http.Client, origin string) string {
	fallback := origin + "/"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/manifest.webmanifest", nil)
	if err != nil {
		return fallback
	}
	response, err := client.Do(request)
	if err != nil {
		return fallback
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fallback
	}
	var manifest struct {
		StartURL string `json:"start_url"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&manifest) != nil || manifest.StartURL == "" {
		return fallback
	}
	base, _ := url.Parse(fallback)
	resolved, err := base.Parse(manifest.StartURL)
	if err != nil || !strings.EqualFold(resolved.Scheme+"://"+resolved.Host, origin) {
		return fallback
	}
	resolved.Fragment = ""
	return resolved.String()
}

// Document declarations come from the same app manifest as permissions.
func manifestDocumentTypes(detail map[string]any) []nativeDocumentType {
	revision, _ := detail["latestRevision"].(map[string]any)
	if revision == nil {
		if revisions, _ := detail["revisions"].([]any); len(revisions) > 0 {
			revision, _ = revisions[0].(map[string]any)
		}
	}
	manifest, _ := revision["manifest"].(map[string]any)
	launch, _ := manifest["launch"].(map[string]any)
	data, _ := json.Marshal(launch["fileTypes"])
	var types []nativeDocumentType
	_ = json.Unmarshal(data, &types)
	return types
}
