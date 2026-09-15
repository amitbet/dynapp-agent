package shellagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSettingsTestServer(t *testing.T, dyner *httptest.Server) (*Server, *httptest.Server) {
	t.Helper()
	authPath := filepath.Join(t.TempDir(), "dyner", "auth.json")
	agent := &Server{Config: Config{DynerBaseURL: dyner.URL}, AccountAuthPath: authPath}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/settings/status", agent.handleSettingsStatus)
	mux.HandleFunc("/api/settings/login/start", agent.handleSettingsLoginStart)
	mux.HandleFunc("/api/settings/login/callback", agent.handleSettingsLoginCallback)
	mux.HandleFunc("/ui/", agent.handleSettingsAsset)
	loopback := httptest.NewServer(mux)
	t.Cleanup(loopback.Close)
	return agent, loopback
}

func TestSettingsAssetsFallBackToHostedDynerApp(t *testing.T) {
	hosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clientSettingsPanels.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte("export const hosted = true;"))
		case "/styles.css":
			_, _ = w.Write([]byte("body{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hosted.Close()
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Dyner request %s", r.URL.Path)
	}))
	defer dyner.Close()
	t.Setenv("DYNAPP_DYNER_UI_ORIGIN", hosted.URL)
	t.Setenv("DYNAPP_DYNER_CONTENT", filepath.Join(t.TempDir(), "missing"))
	_, loopback := newSettingsTestServer(t, dyner)

	response, err := http.Get(loopback.URL + "/ui/clientSettingsPanels.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readAll(response)
	if response.StatusCode != http.StatusOK || string(body) != "export const hosted = true;" {
		t.Fatalf("hosted asset not served: %d %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf("unexpected content type %q", got)
	}
	response, err = http.Get(loopback.URL + "/ui/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet not served: %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	for _, bad := range []string{"../secret.js", "index.html", "styles.css.map", "a%2Fb.js"} {
		response, err = http.Get(loopback.URL + "/ui/" + bad)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%q should be rejected, got %d", bad, response.StatusCode)
		}
	}
}

func TestHostedDynerOriginUsesCatalogHostnameSlug(t *testing.T) {
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/"+DefaultCatalogAppID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"hostname_slug": "amitbet-dyner"}})
	}))
	defer dyner.Close()
	t.Setenv("DYNAPP_DYNER_UI_ORIGIN", "")
	agent := &Server{Config: Config{DynerBaseURL: dyner.URL}}
	origin, err := agent.hostedDynerOrigin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(dyner.URL)
	want := "http://amitbet-dyner." + parsed.Hostname() + ":" + parsed.Port()
	if origin != want {
		t.Fatalf("origin %q, want %q", origin, want)
	}
}

func TestSettingsLoginStoresVerifiedAccountToken(t *testing.T) {
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer issued-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"email": "amit@example.com"}})
	}))
	defer dyner.Close()
	agent, loopback := newSettingsTestServer(t, dyner)

	// A callback without a state issued by this agent is refused.
	response, err := http.Get(loopback.URL + "/api/settings/login/callback?token=issued-token&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state accepted: %d", response.StatusCode)
	}

	start, err := http.Post(loopback.URL+"/api/settings/login/start", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(started.URL)
	if err != nil || login.Path != "/api/cli/login" || !strings.HasPrefix(started.URL, dyner.URL) {
		t.Fatalf("unexpected login URL %q", started.URL)
	}
	callback, err := url.Parse(login.Query().Get("callback"))
	if err != nil || callback.Path != "/api/settings/login/callback" || callback.Query().Get("state") == "" {
		t.Fatalf("unexpected callback %q", login.Query().Get("callback"))
	}
	if callback.Host != strings.TrimPrefix(loopback.URL, "http://") {
		t.Fatalf("callback host %q should be the agent's loopback host", callback.Host)
	}

	rejected, err := http.Get(loopback.URL + "/api/settings/login/callback?token=wrong-token&state=" + url.QueryEscape(callback.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	if rejected.StatusCode != http.StatusBadGateway {
		t.Fatalf("Dyner-rejected token should fail: %d", rejected.StatusCode)
	}
	// The state is single-use, so a fresh one is needed for the real token.
	start, _ = http.Post(loopback.URL+"/api/settings/login/start", "application/json", strings.NewReader("{}"))
	_ = json.NewDecoder(start.Body).Decode(&started)
	login, _ = url.Parse(started.URL)
	callback, _ = url.Parse(login.Query().Get("callback"))
	completed, err := http.Get(loopback.URL + "/api/settings/login/callback?token=issued-token&state=" + url.QueryEscape(callback.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readAll(completed)
	if completed.StatusCode != http.StatusOK || !strings.Contains(string(body), "amit@example.com") {
		t.Fatalf("sign-in did not complete: %d %s", completed.StatusCode, body)
	}
	agent.mu.Lock()
	token := agent.AccountToken
	agent.mu.Unlock()
	if token != "issued-token" {
		t.Fatalf("account token not activated: %q", token)
	}
	stored, err := LoadDynerAccountToken(agent.AccountAuthPath)
	if err != nil || stored != "issued-token" {
		t.Fatalf("account file not written: %q %v", stored, err)
	}
	info, _ := os.Stat(agent.AccountAuthPath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("account file mode %v, want 0600", info.Mode().Perm())
	}
	status, _ := http.Get(loopback.URL + "/api/settings/status")
	var payload map[string]any
	_ = json.NewDecoder(status.Body).Decode(&payload)
	if payload["signedIn"] != true {
		t.Fatalf("status should report signedIn: %v", payload)
	}
}

func readAll(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	var out []byte
	buffer := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buffer)
		out = append(out, buffer[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}
