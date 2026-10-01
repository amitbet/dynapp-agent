package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The dynapp:// URL scheme lets a browser wake a stopped agent: Dyner opens
// dynapp://start after a click and keeps polling the agent. The URL carries no
// authority. It never installs, launches, or grants anything; everything else
// still goes through Dyner's authenticated connection.
const (
	urlScheme         = "dynapp"
	agentServiceName  = "dynapp-shell-agent"
	agentStartTimeout = 15 * time.Second
)

var errUnsupportedURL = errors.New("unsupported dynapp:// URL")

// parseAgentURL accepts dynapp://start (and dynapp:start) only.
func parseAgentURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, urlScheme) {
		return "", errUnsupportedURL
	}
	action := strings.ToLower(strings.Trim(parsed.Host+parsed.Opaque+parsed.Path, "/"))
	if action == "" || action == "start" {
		return "start", nil
	}
	return "", fmt.Errorf("%w: %s", errUnsupportedURL, action)
}

// agentHealthy reports whether an agent answers on the loopback address.
func agentHealthy(ctx context.Context, address string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func waitForAgent(address string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if agentHealthy(context.Background(), address) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// runOpenURL handles one dynapp:// launch: if no agent answers, start the
// installed one (launchd job, Windows service, or a per-user process).
func runOpenURL(raw, address string) error {
	if _, err := parseAgentURL(raw); err != nil {
		return err
	}
	if agentHealthy(context.Background(), address) {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := startInstalledAgent(executable); err != nil {
		return fmt.Errorf("start the DynApp agent: %w", err)
	}
	if !waitForAgent(address, agentStartTimeout) {
		return errors.New("the DynApp agent did not start in time")
	}
	log.Printf("DynApp Shell agent: started from %s", raw)
	return nil
}
