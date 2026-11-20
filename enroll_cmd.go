package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func desktopBrowserAvailable() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	case "linux":
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	default:
		return false
	}
}

// runEnrollment is called from the installing user's session, never from the
// background service. Upgrades do not reopen sign-in for an enrolled agent.
func runEnrollment(address string, onlyIfNeeded bool) error {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || (!strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback())) {
		return errors.New("agent enrollment requires a loopback address")
	}
	if !waitForAgent(address, agentStartTimeout) {
		return errors.New("start the DynApp agent before signing in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	signIn, err := enrollmentURL(ctx, "http://"+address, onlyIfNeeded)
	if err != nil || signIn == "" {
		return err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", signIn)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", signIn)
	default:
		command = exec.Command("xdg-open", signIn)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("open Dyner sign-in in your browser: %w", err)
	}
	return command.Process.Release()
}

func enrollmentURL(ctx context.Context, local string, onlyIfNeeded bool) (string, error) {
	var status struct {
		SignedIn     bool   `json:"signedIn"`
		DynerBaseURL string `json:"dynerBaseUrl"`
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, target any) error {
		req, err := http.NewRequestWithContext(ctx, method, local+path, nil)
		if err != nil {
			return err
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("agent enrollment returned HTTP %d", response.StatusCode)
		}
		return json.NewDecoder(response.Body).Decode(target)
	}
	if err := request(http.MethodGet, "/api/settings/status", &status); err != nil || (onlyIfNeeded && status.SignedIn) {
		return "", err
	}
	var started struct {
		URL string `json:"url"`
	}
	if err := request(http.MethodPost, "/api/settings/login/start", &started); err != nil {
		return "", err
	}
	signIn, err := url.Parse(started.URL)
	dyner, dynerErr := url.Parse(status.DynerBaseURL)
	if err != nil || dynerErr != nil || signIn.User != nil || signIn.Host == "" ||
		signIn.Host != dyner.Host || signIn.Scheme != dyner.Scheme || (signIn.Scheme != "https" && signIn.Scheme != "http") {
		return "", errors.New("the agent returned an invalid Dyner sign-in URL")
	}
	return signIn.String(), nil
}
