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
}

// updateNowVerifier checks a downloaded release's signature; tests stub it.
var updateNowVerifier = verifyDownloadedAgentSignature

// updateNow checks GitHub for a newer agent right away. When one exists it
// installs it in the background, as the hourly check would, skipping the
// backoff after an unfinished attempt because someone asked for it.
func (s *Server) updateNow(ctx context.Context) (UpdateNowResult, error) {
	s.mu.Lock()
	config, enabled := s.selfUpdateConfig, s.selfUpdateCancel != nil
	s.mu.Unlock()
	if !enabled {
		return UpdateNowResult{}, errors.New("self-update is off for this agent (a development build, or started with --no-self-update)")
	}
	result := UpdateNowResult{Current: config.Version}
	release, err := fetchLatestRelease(ctx, config)
	if err != nil {
		return result, fmt.Errorf("check for updates: %w", err)
	}
	result.Latest = normalizedReleaseVersion(release.TagName)
	if !newerRelease(release.TagName, config.Version) {
		return result, nil
	}
	result.Updating = true
	if !s.updateNowRunning.CompareAndSwap(false, true) {
		return result, nil // an update requested earlier is still running
	}
	if path := s.updateAttemptPath(); path != "" {
		_ = os.Remove(path)
	}
	go func() {
		defer s.updateNowRunning.Store(false)
		log.Printf("self-update requested: current=%s latest=%s", config.Version, release.TagName)
		s.checkSelfUpdateWithVerifier(context.Background(), config, updateNowVerifier)
	}()
	return result, nil
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardSettingsRequest(w, r, true) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.updateNow(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "current": result.Current})
		return
	}
	writeJSON(w, result)
}

// RequestUpdate asks the agent listening on address to update itself now.
func RequestUpdate(ctx context.Context, address string) (UpdateNowResult, error) {
	var result UpdateNowResult
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/api/settings/update", strings.NewReader("{}"))
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
