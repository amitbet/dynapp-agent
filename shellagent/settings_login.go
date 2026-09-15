package shellagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The settings page reuses the Dyner client's settings panels. A development
// checkout serves them from apps/dyner/content on disk; an installed agent has
// no checkout, so it fetches the same files from the hosted Dyner app and
// caches them for the life of the process.

const settingsLoginStateTTL = 10 * time.Minute

var settingsAssetName = regexp.MustCompile(`^[A-Za-z0-9_-]+\.(js|css)$`)

type settingsAsset struct {
	contentType string
	data        []byte
}

func (s *Server) settingsAssetContent(ctx context.Context, name string) (settingsAsset, error) {
	if !settingsAssetName.MatchString(name) {
		return settingsAsset{}, os.ErrNotExist
	}
	contentType := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	}
	if data, err := loadDynerContentFile(name); err == nil {
		return settingsAsset{contentType: contentType, data: data}, nil
	}
	s.settingsAssetMu.Lock()
	cached, ok := s.settingsAssetCache[name]
	s.settingsAssetMu.Unlock()
	if ok {
		return cached, nil
	}
	origin, err := s.hostedDynerOrigin(ctx)
	if err != nil {
		return settingsAsset{}, err
	}
	data, err := fetchHostedAsset(ctx, s.DynerHTTPClient, origin+"/"+name)
	if err != nil {
		return settingsAsset{}, err
	}
	asset := settingsAsset{contentType: contentType, data: data}
	s.settingsAssetMu.Lock()
	if s.settingsAssetCache == nil {
		s.settingsAssetCache = map[string]settingsAsset{}
	}
	s.settingsAssetCache[name] = asset
	s.settingsAssetMu.Unlock()
	return asset, nil
}

// hostedDynerOrigin resolves the hosted origin of the Dyner client app, for
// example https://amitbet-dyner.dynapp.io, from the catalog record's hostname
// slug and the Dyner server's domain.
func (s *Server) hostedDynerOrigin(ctx context.Context) (string, error) {
	s.settingsAssetMu.Lock()
	origin := s.settingsAssetOrigin
	s.settingsAssetMu.Unlock()
	if origin != "" {
		return origin, nil
	}
	s.mu.Lock()
	base := s.Config.DynerBaseURL
	s.mu.Unlock()
	if override := strings.TrimSpace(os.Getenv("DYNAPP_DYNER_UI_ORIGIN")); override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("Dyner base URL is not configured")
	}
	endpoint, err := dynerAPIURL(base, "/api/v1/apps/"+DefaultCatalogAppID)
	if err != nil {
		return "", err
	}
	data, status, err := dynerJSON(ctx, s.DynerHTTPClient, http.MethodGet, endpoint, "", nil)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("Dyner app lookup failed: HTTP %d", status)
	}
	var payload struct {
		App struct {
			HostnameSlug string `json:"hostname_slug"`
		} `json:"app"`
		HostnameSlug string `json:"hostname_slug"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	slug := payload.App.HostnameSlug
	if slug == "" {
		slug = payload.HostnameSlug
	}
	if slug == "" {
		return "", errors.New("Dyner did not report a hosted hostname for its client app")
	}
	domain := strings.TrimPrefix(parsed.Hostname(), "www.")
	origin = parsed.Scheme + "://" + slug + "." + domain
	if port := parsed.Port(); port != "" {
		origin += ":" + port
	}
	s.settingsAssetMu.Lock()
	s.settingsAssetOrigin = origin
	s.settingsAssetMu.Unlock()
	return origin, nil
}

func fetchHostedAsset(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("hosted asset %s: HTTP %d", rawURL, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 4<<20))
}

// Browser sign-in reuses Dyner's CLI login: the agent hands the browser a
// Dyner URL with a loopback callback, Dyner appends the account token after
// Google sign-in, and the callback stores it where the agent already looks.
// A random state bound to the callback stops a foreign page from logging the
// agent into an attacker's account by redirecting the browser here.

func (s *Server) handleSettingsLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodPost {
		return
	}
	s.mu.Lock()
	base := s.Config.DynerBaseURL
	s.mu.Unlock()
	if base == "" {
		http.Error(w, "Dyner server is not configured.", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	s.loginMu.Lock()
	if s.loginStates == nil {
		s.loginStates = map[string]time.Time{}
	}
	for key, issued := range s.loginStates {
		if time.Since(issued) > settingsLoginStateTTL {
			delete(s.loginStates, key)
		}
	}
	s.loginStates[state] = time.Now()
	s.loginMu.Unlock()
	callback := url.URL{Scheme: "http", Host: r.Host, Path: "/api/settings/login/callback", RawQuery: url.Values{"state": {state}}.Encode()}
	login := strings.TrimRight(base, "/") + "/api/cli/login?" + url.Values{"callback": {callback.String()}}.Encode()
	writeJSON(w, map[string]any{"url": login})
}

func (s *Server) consumeLoginState(state string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	issued, ok := s.loginStates[state]
	if ok {
		delete(s.loginStates, state)
	}
	return ok && time.Since(issued) <= settingsLoginStateTTL
}

func (s *Server) handleSettingsLoginCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsLoopback(w, r) || r.Method != http.MethodGet {
		return
	}
	query := r.URL.Query()
	token := strings.TrimSpace(query.Get("token"))
	if token == "" || !s.consumeLoginState(query.Get("state")) {
		http.Error(w, "Dyner sign-in did not complete. Start again from the settings page.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	base := s.Config.DynerBaseURL
	s.mu.Unlock()
	email, err := s.storeAccountToken(r.Context(), base, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html><meta http-equiv="refresh" content="1; url=/settings"><title>Signed in</title><h1>Signed in as `+html.EscapeString(email)+`</h1><p>Returning to the agent settings…</p>`)
}

// storeAccountToken verifies the token against Dyner, persists it in the same
// account file the agent reads at startup, and activates it for this process.
func (s *Server) storeAccountToken(ctx context.Context, base, token string) (string, error) {
	endpoint, err := dynerAPIURL(base, "/api/v1/me")
	if err != nil {
		return "", err
	}
	data, status, err := dynerJSON(ctx, s.DynerHTTPClient, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("Dyner rejected the sign-in token: HTTP %d", status)
	}
	var me struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &me); err != nil {
		return "", err
	}
	if me.User.Email == "" {
		return "", errors.New("Dyner did not return an account email for the sign-in token")
	}
	authPath := s.AccountAuthPath
	if authPath == "" {
		authPath, err = DefaultDynerAuthPath()
		if err != nil {
			return "", err
		}
	}
	var record dynerAccountFile
	record.Token = token
	record.Account.User.Email = me.User.Email
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(authPath, encoded, 0o600); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.AccountToken = token
	s.mu.Unlock()
	return me.User.Email, nil
}
