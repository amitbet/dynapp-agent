package shellagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Settings CSRF: the settings HTML embeds a per-process token in
// `<meta name="dynapp-settings-csrf">`; browser callers echo it in
// `X-Dynapp-Settings-Csrf` on every mutating request.
const (
	settingsCSRFHeader      = "X-Dynapp-Settings-Csrf"
	settingsCSRFPlaceholder = "__DYNAPP_SETTINGS_CSRF__"
)

//go:embed ui/index.html ui/bridge.js
var settingsUI embed.FS

func (s *Server) registerSettingsHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleSettingsIndex)
	mux.HandleFunc("/settings", s.handleSettingsIndex)
	mux.HandleFunc("/ui/", s.handleSettingsAsset)
	mux.HandleFunc("/api/settings/status", s.handleSettingsStatus)
	mux.HandleFunc("/api/settings/remote-environments", s.handleSettingsRemoteList)
	mux.HandleFunc("/api/settings/remote-environments/server", s.handleSettingsSetServer)
	mux.HandleFunc("/api/settings/remote-environments/relay", s.handleSettingsSetRelay)
	mux.HandleFunc("/api/settings/remote-environments/tailscale", s.handleSettingsTailscale)
	mux.HandleFunc("/api/settings/remote-environments/rename", s.handleSettingsRename)
	mux.HandleFunc("/api/settings/remote-environments/remove", s.handleSettingsRemove)
	mux.HandleFunc("/api/v1/", s.handleSettingsDynerProxy)
}

func (s *Server) settingsCSRFToken() string {
	s.csrfMu.Lock()
	defer s.csrfMu.Unlock()
	if s.csrfToken == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err == nil {
			s.csrfToken = base64.RawURLEncoding.EncodeToString(raw)
		}
	}
	return s.csrfToken
}

// ownOrigin reports whether a browser Origin header names this agent's own
// loopback origin (`http://127.0.0.1:<port>` or `http://localhost:<port>`).
func ownOrigin(r *http.Request, origin string) bool {
	parsed, ok := parseOrigin(origin)
	if !ok || parsed.Scheme != "http" || !loopbackHostname(parsed.Hostname()) {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host) || parsed.Port() == requestPort(r.Host)
}

func requestPort(host string) string {
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return ""
	}
	return port
}

// guardSettingsRequest applies the settings API rules: loopback peer and
// Host, own-origin when a browser sends Origin, and for mutations a JSON body
// plus the CSRF token for browser callers. Non-browser callers (launcher,
// CLI) send no Origin and are accepted with a JSON content type.
func (s *Server) guardSettingsRequest(w http.ResponseWriter, r *http.Request, mutating bool) bool {
	if !loopbackRequest(r) || !loopbackHostHeader(r.Host) {
		http.Error(w, "settings are only available on loopback", http.StatusForbidden)
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && origin != "null" && !ownOrigin(r, origin) {
		http.Error(w, "origin is not allowed to use agent settings", http.StatusForbidden)
		return false
	}
	if !mutating {
		return true
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		http.Error(w, "mutating settings requests must send application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if origin != "" {
		token := s.settingsCSRFToken()
		provided := strings.TrimSpace(r.Header.Get(settingsCSRFHeader))
		if token == "" || provided == "" || subtle.ConstantTimeCompare([]byte(token), []byte(provided)) != 1 {
			http.Error(w, "settings CSRF token is missing or invalid", http.StatusForbidden)
			return false
		}
	}
	return true
}

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) requireSettingsLoopback(w http.ResponseWriter, r *http.Request) bool {
	mutating := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	return s.guardSettingsRequest(w, r, mutating)
}

func (s *Server) handleSettingsIndex(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) {
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/settings" {
		http.NotFound(w, r)
		return
	}
	data, err := settingsUI.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = bytes.ReplaceAll(data, []byte(settingsCSRFPlaceholder), []byte(s.settingsCSRFToken()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleSettingsAsset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "bridge.js" {
		data, err := settingsUI.ReadFile("ui/bridge.js")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	if name == "clientSettingsPanels.js" || name == "styles.css" {
		data, err := loadDynerContentFile(name)
		if err != nil {
			http.Error(w, "Dyner settings assets were not found: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(name, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		_, _ = w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func loadDynerContentFile(name string) ([]byte, error) {
	name = filepath.Base(name)
	if dir := strings.TrimSpace(os.Getenv("DYNAPP_DYNER_CONTENT")); dir != "" {
		return os.ReadFile(filepath.Join(dir, name))
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, dynerContentCandidates(cwd, name)...)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, dynerContentCandidates(filepath.Dir(exe), name)...)
	}
	var last error
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		last = err
	}
	if last == nil {
		last = fs.ErrNotExist
	}
	return nil, last
}

func dynerContentCandidates(start, name string) []string {
	var paths []string
	dir := start
	for i := 0; i < 8; i++ {
		paths = append(paths, filepath.Join(dir, "apps", "dyner", "content", name))
		paths = append(paths, filepath.Join(dir, "dynapp", "apps", "dyner", "content", name))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return paths
}

func (s *Server) handleSettingsStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodGet {
		if r.Method != http.MethodGet && loopbackRequest(r) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	s.mu.Lock()
	config := s.Config
	signedIn := s.AccountToken != ""
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"ok": true, "signedIn": signedIn, "dynerBaseUrl": config.DynerBaseURL,
		"redacted": config.Redacted(), "settings": "/settings",
	})
}

func (s *Server) handleSettingsRemoteList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodGet {
		return
	}
	payload, err := s.settingsRemotePayload(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, payload)
}

func (s *Server) settingsRemotePayload(ctx context.Context) (map[string]any, error) {
	server, err := s.lanStatus()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	config := s.Config
	token := s.AccountToken
	s.mu.Unlock()
	environments := []any{}
	if token != "" && config.DynerBaseURL != "" {
		listed, listErr := ListRemoteEnvironments(ctx, nil, config.DynerBaseURL, token)
		if listErr != nil {
			return nil, listErr
		}
		for _, environment := range listed {
			item := map[string]any{
				"id": environment.ID, "name": environment.Name, "displayName": environment.Name,
				"state": environment.State, "endpoint": environment.Endpoint,
				"detail": environment.Endpoint, "capabilities": environment.Capabilities,
				"transport": environment.Transport,
			}
			environments = append(environments, item)
		}
	}
	relayState := "Disabled"
	if config.RelayEnabled {
		relayState = "Reconnecting"
	}
	var listenerRegistration any
	if config.DeviceCredential != "" {
		listenerRegistration = map[string]any{
			"environmentId": config.EnvironmentID, "name": config.ListenerName,
			"enabled":   config.ListenerMode != ListenerOff,
			"transport": map[string]any{"kind": "lan", "endpoint": config.ListenerEndpoint()},
		}
	}
	var hostedRegistration any
	if config.HostedDeviceCredential != "" {
		hostedRegistration = map[string]any{
			"environmentId": config.HostedEnvironmentID, "name": config.HostedName,
			"enabled": config.RelayEnabled, "transport": map[string]any{"kind": "relay", "provider": "cloudflare"},
		}
	}
	return map[string]any{
		"environments": environments, "server": server,
		"relay":                map[string]any{"supported": true, "enabled": config.RelayEnabled, "connected": false, "state": relayState},
		"listenerRegistration": listenerRegistration, "hostedRegistration": hostedRegistration,
		"pairingLinks": []any{},
	}, nil
}

func (s *Server) handleSettingsSetServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodPost {
		return
	}
	var body struct {
		Enabled      bool   `json:"enabled"`
		ListenerMode string `json:"listenerMode"`
		Name         string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := body.ListenerMode
	if !body.Enabled {
		mode = ListenerOff
	} else if mode == "" || mode == ListenerOff {
		mode = ListenerLocal
	}
	status, err := s.setListener(mode, body.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, status)
}

func (s *Server) handleSettingsSetRelay(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodPost {
		return
	}
	var body struct {
		Enabled bool   `json:"enabled"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.ensureHostedRegistration(r.Context(), body.Enabled, body.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	payload, err := s.settingsRemotePayload(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, payload)
}

func (s *Server) handleSettingsTailscale(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) {
		return
	}
	http.Error(w, "Tailscale is not available in the headless Shell agent yet.", http.StatusNotImplemented)
}

func (s *Server) handleSettingsRename(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodPost {
		return
	}
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	config := s.Config
	token := s.AccountToken
	s.mu.Unlock()
	if err := UpdateRemoteEnvironment(r.Context(), nil, config.DynerBaseURL, token, body.ID, map[string]any{"name": body.Name}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if body.ID == config.EnvironmentID {
		s.mu.Lock()
		s.Config.ListenerName = body.Name
		snapshot := s.Config
		s.mu.Unlock()
		_ = SaveConfig(s.StateDir, snapshot)
	}
	if body.ID == config.HostedEnvironmentID {
		s.mu.Lock()
		s.Config.HostedName = body.Name
		snapshot := s.Config
		s.mu.Unlock()
		_ = SaveConfig(s.StateDir, snapshot)
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSettingsRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodPost {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	config := s.Config
	token := s.AccountToken
	s.mu.Unlock()
	if err := RevokeRemoteEnvironment(r.Context(), nil, config.DynerBaseURL, token, body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSettingsDynerProxy(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) {
		return
	}
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/v1/workspaces") {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	base := s.Config.DynerBaseURL
	token := s.AccountToken
	s.mu.Unlock()
	if token == "" || base == "" {
		http.Error(w, "Sign in to Dyner first.", http.StatusUnauthorized)
		return
	}
	endpoint, err := dynerAPIURL(base, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.RawQuery != "" {
		endpoint += "?" + r.URL.RawQuery
	}
	var body any
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}
	data, status, err := dynerJSON(r.Context(), nil, r.Method, endpoint, token, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
