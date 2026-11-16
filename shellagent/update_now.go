package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// UpdateNowResult is what `dynapp-shell-agent --update` and the settings
// API report: the running and latest versions, and whether an update to the
// latest one is now being installed (the agent restarts when it finishes).
type UpdateNowResult struct {
	Current  string `json:"current"`
	Latest   string `json:"latest"`
	Updating bool   `json:"updating"`
	// ActiveBridges counts the TCP, UDP and RDP bridges open in the agent.
	// An update waits for them to close unless it was forced.
	ActiveBridges int `json:"activeBridges"`
}

// UpdateStatus is what GET /api/settings/update reports while
// `dynapp-shell-agent --update` waits for the new version.
type UpdateStatus struct {
	Current       string `json:"current"`
	Updating      bool   `json:"updating"`
	ActiveBridges int    `json:"activeBridges"`
	// WaitingForBridges is set while a downloaded update waits for active
	// bridges to close; BridgeWaitUntil is when it applies anyway.
	WaitingForBridges bool      `json:"waitingForBridges"`
	BridgeWaitUntil   time.Time `json:"bridgeWaitUntil,omitzero"`
}

// updateNowVerifier checks a downloaded release's signature; tests stub it.
var updateNowVerifier = verifyDownloadedAgentSignature

// updateNow checks GitHub for a newer agent right away. When one exists it
// installs it in the background, as the hourly check would, skipping the
// backoff after an unfinished attempt because someone asked for it. It still
// waits for active bridges to close unless force is set; force also ends a
// wait that is already under way.
func (s *Server) updateNow(ctx context.Context, force bool) (UpdateNowResult, error) {
	s.mu.Lock()
	config, enabled := s.selfUpdateConfig, s.selfUpdateCancel != nil
	s.mu.Unlock()
	if !enabled {
		return UpdateNowResult{}, errors.New("self-update is off for this agent (a development build, or started with --no-self-update)")
	}
	result := UpdateNowResult{Current: config.Version, ActiveBridges: s.ActiveBridges()}
	release, err := fetchLatestRelease(ctx, config)
	if err != nil {
		return result, fmt.Errorf("check for updates: %w", err)
	}
	result.Latest = normalizedReleaseVersion(release.TagName)
	if !newerRelease(release.TagName, config.Version) {
		return result, nil
	}
	result.Updating = true
	if force {
		s.updateForce.Store(true)
	}
	if !s.updateNowRunning.CompareAndSwap(false, true) {
		return result, nil // an update requested earlier is still running
	}
	if path := s.updateAttemptPath(); path != "" {
		_ = os.Remove(path)
	}
	go func() {
		defer s.updateNowRunning.Store(false)
		// Still running here means the update did not restart the agent; a
		// later scheduled update must wait for bridges again.
		defer s.updateForce.Store(false)
		log.Printf("self-update requested: current=%s latest=%s force=%t", config.Version, release.TagName, force)
		s.checkSelfUpdateWithVerifier(context.Background(), config, updateNowVerifier)
	}()
	return result, nil
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.guardSettingsRequest(w, r, false) {
			return
		}
		writeJSON(w, s.updateStatus())
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardSettingsRequest(w, r, true) {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.updateNow(ctx, body.Force)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "current": result.Current})
		return
	}
	writeJSON(w, result)
}

func (s *Server) updateStatus() UpdateStatus {
	s.mu.Lock()
	current := s.selfUpdateConfig.Version
	s.mu.Unlock()
	if current == "" {
		current = AgentVersion
	}
	s.updateMu.RLock()
	pending := s.updatePending
	s.updateMu.RUnlock()
	status := UpdateStatus{
		Current:       current,
		Updating:      s.updateNowRunning.Load() || pending,
		ActiveBridges: s.ActiveBridges(),
	}
	if deadline := s.bridgeWaitDeadline(); !deadline.IsZero() {
		status.WaitingForBridges = true
		status.BridgeWaitUntil = deadline
		status.Updating = true
	}
	return status
}

// RequestUpdate asks the agent listening on address to update itself now.
// With force it closes active bridges instead of waiting for them.
func RequestUpdate(ctx context.Context, address string, force bool) (UpdateNowResult, error) {
	var result UpdateNowResult
	payload := "{}"
	if force {
		payload = `{"force":true}`
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/api/settings/update", strings.NewReader(payload))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return result, fmt.Errorf("no DynApp agent is answering on %s: %w", address, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	isJSON := strings.HasPrefix(response.Header.Get("Content-Type"), "application/json")
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || (response.StatusCode == http.StatusOK && !isJSON) {
		return result, fmt.Errorf("the agent on %s is too old to update on request; restart it and it updates at startup", address)
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Error != "" {
			return result, errors.New(failure.Error)
		}
		return result, fmt.Errorf("agent on %s: HTTP %d: %s", address, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return result, json.Unmarshal(body, &result)
}

// FetchUpdateStatus reads GET /api/settings/update. ok is false when no agent
// answers or the agent predates the status endpoint.
func FetchUpdateStatus(ctx context.Context, address string) (status UpdateStatus, ok bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/settings/update", nil)
	if err != nil {
		return status, false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return status, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return status, false
	}
	return status, json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status) == nil
}

// RunningAgentVersion is the version /healthz reports, or "" when no agent
// answers (for example while it restarts into an update).
func RunningAgentVersion(ctx context.Context, address string) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		return ""
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	var health struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health) != nil {
		return ""
	}
	return normalizedReleaseVersion(health.Version)
}
