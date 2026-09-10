package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type remoteEnvironmentRecord struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Endpoint     string         `json:"endpoint"`
	State        string         `json:"state"`
	Capabilities []string       `json:"capabilities"`
	Transport    map[string]any `json:"transport"`
}

type registerEnvironmentResponse struct {
	Environment      remoteEnvironmentRecord `json:"environment"`
	DeviceCredential string                  `json:"deviceCredential"`
}

func dynerJSON(ctx context.Context, client *http.Client, method, rawURL, bearer string, body any) ([]byte, int, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, 0, err
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return data, response.StatusCode, nil
}

func dynerAPIURL(base, path string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func RegisterRemoteEnvironment(ctx context.Context, client *http.Client, baseURL, bearer string, body map[string]any) (registerEnvironmentResponse, error) {
	endpoint, err := dynerAPIURL(baseURL, "/api/v1/remote-environments")
	if err != nil {
		return registerEnvironmentResponse{}, err
	}
	data, status, err := dynerJSON(ctx, client, http.MethodPost, endpoint, bearer, body)
	if err != nil {
		return registerEnvironmentResponse{}, err
	}
	if status < 200 || status >= 300 {
		return registerEnvironmentResponse{}, fmt.Errorf("Dyner environment registration failed: HTTP %d", status)
	}
	var payload registerEnvironmentResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return registerEnvironmentResponse{}, err
	}
	if payload.Environment.ID == "" || payload.DeviceCredential == "" {
		return registerEnvironmentResponse{}, fmt.Errorf("Dyner did not return a device credential")
	}
	return payload, nil
}

func UpdateRemoteEnvironment(ctx context.Context, client *http.Client, baseURL, bearer, environmentID string, body map[string]any) error {
	endpoint, err := dynerAPIURL(baseURL, "/api/v1/remote-environments/"+url.PathEscape(environmentID))
	if err != nil {
		return err
	}
	_, status, err := dynerJSON(ctx, client, http.MethodPatch, endpoint, bearer, body)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errEnvironmentNotFound
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Dyner environment update failed: HTTP %d", status)
	}
	return nil
}

// errEnvironmentNotFound means Dyner no longer knows the environment id the
// agent saved (revoked or deleted server-side). The agent must register again
// instead of retrying the update forever.
var errEnvironmentNotFound = errors.New("Dyner environment update failed: HTTP 404 (environment no longer exists)")

func RevokeRemoteEnvironment(ctx context.Context, client *http.Client, baseURL, bearer, environmentID string) error {
	if environmentID == "" {
		return nil
	}
	endpoint, err := dynerAPIURL(baseURL, "/api/v1/remote-environments/"+url.PathEscape(environmentID))
	if err != nil {
		return err
	}
	_, status, err := dynerJSON(ctx, client, http.MethodDelete, endpoint, bearer, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 && status != http.StatusNotFound && status != http.StatusGone {
		return fmt.Errorf("Dyner environment revoke failed: HTTP %d", status)
	}
	return nil
}

func ListRemoteEnvironments(ctx context.Context, client *http.Client, baseURL, bearer string) ([]remoteEnvironmentRecord, error) {
	endpoint, err := dynerAPIURL(baseURL, "/api/v1/remote-environments")
	if err != nil {
		return nil, err
	}
	data, status, err := dynerJSON(ctx, client, http.MethodGet, endpoint, bearer, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Dyner environment list failed: HTTP %d", status)
	}
	var payload struct {
		Environments []remoteEnvironmentRecord `json:"environments"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload.Environments, nil
}

func (s *Server) certificateHashes() ([]map[string]string, error) {
	hash, err := s.LANCertificateHash()
	if err != nil {
		return nil, err
	}
	return []map[string]string{hash}, nil
}

func (s *Server) ensureListenerRegistration(ctx context.Context) error {
	s.mu.Lock()
	config := s.Config
	token := s.AccountToken
	s.mu.Unlock()
	if config.ListenerMode == ListenerOff || token == "" || config.DynerBaseURL == "" {
		return nil
	}
	hashes, err := s.certificateHashes()
	if err != nil {
		return err
	}
	endpoint := config.ListenerEndpoint()
	name := config.ListenerName
	if name == "" {
		name = DefaultListenerName()
	}
	capabilities := DefaultListenerCapabilities()
	transport := map[string]any{
		"kind":                    "lan",
		"endpoint":                endpoint,
		"serverCertificateHashes": hashes,
	}
	if config.EnvironmentID != "" && config.DeviceCredential != "" {
		err := UpdateRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, config.EnvironmentID, map[string]any{
			"name":                    name,
			"endpoint":                endpoint,
			"transport":               transport,
			"serverCertificateHashes": hashes,
			"capabilities":            capabilities,
		})
		if !errors.Is(err, errEnvironmentNotFound) {
			return err
		}
		// The saved environment was revoked or deleted on Dyner; drop the stale
		// device credential and register this listener as a new environment.
		log.Printf("DynApp Shell agent: environment %s no longer exists on Dyner; registering again", config.EnvironmentID)
		s.mu.Lock()
		s.Config.EnvironmentID = ""
		s.Config.DeviceCredential = ""
		s.mu.Unlock()
	}
	created, err := RegisterRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, map[string]any{
		"name":                    name,
		"endpoint":                endpoint,
		"transport":               transport,
		"serverCertificateHashes": hashes,
		"capabilities":            capabilities,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.Config.EnvironmentID = created.Environment.ID
	s.Config.DeviceCredential = created.DeviceCredential
	s.Config.ListenerName = created.Environment.Name
	if created.Environment.Name == "" {
		s.Config.ListenerName = name
	}
	snapshot := s.Config
	s.mu.Unlock()
	return SaveConfig(s.StateDir, snapshot)
}

func (config Config) listenerTransportEndpoint() (string, bool) {
	return config.ListenerEndpoint(), config.DeviceCredential != ""
}

func (s *Server) ensureHostedRegistration(ctx context.Context, enabled bool, name string) error {
	s.mu.Lock()
	config := s.Config
	token := s.AccountToken
	s.mu.Unlock()
	if token == "" || config.DynerBaseURL == "" {
		return fmt.Errorf("sign in to Dyner first (same account credential Electron stores in dyner/auth.json)")
	}
	if !enabled {
		if config.HostedEnvironmentID != "" {
			_ = RevokeRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, config.HostedEnvironmentID)
		}
		s.mu.Lock()
		s.Config.RelayEnabled = false
		s.Config.HostedEnvironmentID = ""
		s.Config.HostedDeviceCredential = ""
		snapshot := s.Config
		s.mu.Unlock()
		s.stopRelay()
		return SaveConfig(s.StateDir, snapshot)
	}
	if strings.TrimSpace(name) == "" {
		name = DefaultListenerName()
	}
	capabilities := DefaultListenerCapabilities()
	if config.HostedEnvironmentID != "" && config.HostedDeviceCredential != "" {
		_ = UpdateRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, config.HostedEnvironmentID, map[string]any{
			"name":              name,
			"capabilities":      capabilities,
			"relayShellEnabled": true,
		})
		s.mu.Lock()
		s.Config.RelayEnabled = true
		s.Config.HostedName = name
		snapshot := s.Config
		s.mu.Unlock()
		if err := SaveConfig(s.StateDir, snapshot); err != nil {
			return err
		}
		s.startRelay()
		return nil
	}
	created, err := RegisterRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, map[string]any{
		"name":         name,
		"transport":    map[string]any{"kind": "relay", "provider": "cloudflare"},
		"capabilities": capabilities,
	})
	if err != nil {
		return err
	}
	_ = UpdateRemoteEnvironment(ctx, nil, config.DynerBaseURL, token, created.Environment.ID, map[string]any{
		"relayShellEnabled": true,
	})
	s.mu.Lock()
	s.Config.RelayEnabled = true
	s.Config.HostedEnvironmentID = created.Environment.ID
	s.Config.HostedDeviceCredential = created.DeviceCredential
	s.Config.HostedName = created.Environment.Name
	if s.Config.HostedName == "" {
		s.Config.HostedName = name
	}
	snapshot := s.Config
	s.mu.Unlock()
	if err := SaveConfig(s.StateDir, snapshot); err != nil {
		return err
	}
	s.startRelay()
	return nil
}
