package shellagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureListenerRegistrationCreatesDeviceCredential(t *testing.T) {
	var registered map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/remote-environments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer user-token") {
			t.Fatal("missing user bearer token")
		}
		if err := json.NewDecoder(r.Body).Decode(&registered); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"environment":{"id":"env_created","name":"Amits-MacBook-Pro.local","transport":{"kind":"lan"}},"deviceCredential":"env_created.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"}`))
	}))
	defer server.Close()
	agent := &Server{
		StateDir:     t.TempDir(),
		AccountToken: "user-token",
		Config: Config{
			DynerBaseURL: server.URL,
			ListenerMode: ListenerLocal,
			LANEnabled:   true,
			LANAddress:   "127.0.0.1:9011",
			ListenerName: "Amits-MacBook-Pro.local",
		},
	}
	if err := agent.ensureListenerRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.Config.EnvironmentID != "env_created" || !strings.HasPrefix(agent.Config.DeviceCredential, "env_created.") {
		t.Fatalf("config = %#v", agent.Config)
	}
	if registered["name"] != "Amits-MacBook-Pro.local" {
		t.Fatalf("registered = %#v", registered)
	}
	transport, _ := registered["transport"].(map[string]any)
	if transport["kind"] != "lan" || transport["endpoint"] != "https://127.0.0.1:9011/dynapp-remote" {
		t.Fatalf("transport = %#v", transport)
	}
}

func TestEnsureListenerRegistrationRefreshesEndpoint(t *testing.T) {
	var updated map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/remote-environments/env_existing" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	agent := &Server{
		StateDir:     t.TempDir(),
		AccountToken: "user-token",
		Config: Config{
			DynerBaseURL:     server.URL,
			EnvironmentID:    "env_existing",
			DeviceCredential: "env_existing.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
			ListenerMode:     ListenerLocal,
			LANEnabled:       true,
			LANAddress:       "127.0.0.1:52971",
			ListenerName:     "This host",
		},
	}
	if err := agent.ensureListenerRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if updated["endpoint"] != "https://127.0.0.1:52971/dynapp-remote" {
		t.Fatalf("updated endpoint = %#v", updated["endpoint"])
	}
	transport, _ := updated["transport"].(map[string]any)
	if transport["endpoint"] != updated["endpoint"] {
		t.Fatalf("updated transport = %#v", transport)
	}
}

func TestLoadDynerAccountToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"token":"secret-token","account":{"user":{"email":"amit@example.test"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := LoadDynerAccountToken(path)
	if err != nil || token != "secret-token" {
		t.Fatalf("token = %q, %v", token, err)
	}
}

func TestSettingsUIServesDynerPanels(t *testing.T) {
	if _, err := loadDynerContentFile("clientSettingsPanels.js"); err != nil {
		t.Skip("Dyner settings assets are not in this checkout")
	}
	agent := &Server{StateDir: t.TempDir(), Config: Config{ListenerMode: ListenerLocal, LANEnabled: true, LANAddress: "127.0.0.1:9011"}}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	panels, err := http.Get(server.URL + "/ui/clientSettingsPanels.js")
	if err != nil {
		t.Fatal(err)
	}
	defer panels.Body.Close()
	if panels.StatusCode != http.StatusOK {
		t.Fatalf("panels status = %d", panels.StatusCode)
	}
}

func TestEnsureListenerRegistrationReregistersWhenEnvironmentIsGone(t *testing.T) {
	var patched, created bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/remote-environments/env_stale":
			patched = true
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/remote-environments":
			created = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"environment":{"id":"env_fresh","name":"This host"},"deviceCredential":"env_fresh.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"}`))
		default:
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	agent := &Server{
		StateDir:     t.TempDir(),
		AccountToken: "user-token",
		Config: Config{
			SchemaVersion:    ConfigSchemaVersion,
			DynerBaseURL:     server.URL,
			EnvironmentID:    "env_stale",
			DeviceCredential: "env_stale.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789",
			ListenerMode:     ListenerLocal,
			LANEnabled:       true,
			LANAddress:       "127.0.0.1:52972",
			ListenerName:     "This host",
		},
	}
	if err := agent.ensureListenerRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !patched || !created {
		t.Fatalf("patched = %v created = %v", patched, created)
	}
	if agent.Config.EnvironmentID != "env_fresh" || agent.Config.DeviceCredential == "" {
		t.Fatalf("config not refreshed: %#v", agent.Config.EnvironmentID)
	}
}
