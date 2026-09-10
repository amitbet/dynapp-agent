package shellagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDefaultListenerCapabilitiesMatchAgentCapabilities(t *testing.T) {
	registered := DefaultListenerCapabilities()
	for _, required := range capabilities() {
		if !slices.Contains(registered, required) {
			t.Fatalf("default listener capabilities omit supported capability %s: %v", required, registered)
		}
	}
	for _, advertised := range registered {
		if !slices.Contains(capabilities(), advertised) {
			t.Fatalf("default listener advertises unsupported capability %s: %v", advertised, registered)
		}
	}
}

func TestConfigIsPrivateAtomicAndRedacted(t *testing.T) {
	stateDir := t.TempDir()
	credential := "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"
	config := Config{DynerBaseURL: "https://dyner.example", EnvironmentID: "env_test", DeviceCredential: credential, RelayEnabled: true}
	if err := SaveConfig(stateDir, config); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeviceCredential != credential || !loaded.RelayEnabled {
		t.Fatalf("loaded config = %#v", loaded)
	}
	path, err := ConfigPath(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
	if got := loaded.Redacted(); got["enrolled"] != true || got["environmentId"] != "env_test" {
		t.Fatalf("redacted = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, ".config-unused")); !os.IsNotExist(err) {
		t.Fatal("unexpected config staging state")
	}
}

func TestConfigRejectsInsecureAndMismatchedEnrollment(t *testing.T) {
	if err := SaveConfig(t.TempDir(), Config{DynerBaseURL: "http://example.test", EnvironmentID: "env_test", DeviceCredential: "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"}); err == nil {
		t.Fatal("expected insecure URL rejection")
	}
	if err := SaveConfig(t.TempDir(), Config{DynerBaseURL: "https://dyner.example", EnvironmentID: "env_one", DeviceCredential: "env_two.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"}); err == nil {
		t.Fatal("expected mismatched credential rejection")
	}
}

func TestConfigAllowsPrivateDynerHTTPAndDefaultLocalListener(t *testing.T) {
	config := Config{}
	config.ApplyListenerDefaults("127.0.0.1:9011", false)
	if config.ListenerMode != ListenerLocal || config.LANAddress != "127.0.0.1:9011" || !config.LANEnabled {
		t.Fatalf("local defaults = %#v", config)
	}
	if !AllowedDynerBaseURL("http://192.168.1.22:9009") || !AllowedDynerBaseURL("https://dyner.example") {
		t.Fatal("expected local HTTP and HTTPS Dyner URLs to be allowed")
	}
	config.ApplyListenerDefaults("127.0.0.1:9011", true)
	if config.ListenerMode != ListenerLAN || config.LANAddress != "0.0.0.0:9011" {
		t.Fatalf("lan defaults = %#v", config)
	}
	config.DynerBaseURL = "http://192.168.1.22:9009"
	if err := SaveConfig(t.TempDir(), config); err != nil {
		t.Fatal(err)
	}
}
