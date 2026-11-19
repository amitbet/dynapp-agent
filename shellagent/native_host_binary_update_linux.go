//go:build linux && !android

package shellagent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// updateNativeHostBinary keeps a dynapp-app-host that sits next to the agent
// at the agent's release. install.sh puts both in one folder and the agent
// updates only itself, so without this the host would stay at the version
// first installed. Package managers own hosts elsewhere (/usr/bin).
func (s *Server) updateNativeHostBinary() {
	s.mu.Lock()
	config := s.selfUpdateConfig
	s.mu.Unlock()
	if !config.Enabled || !isReleaseVersion(config.Version) {
		return
	}
	host := nativeHostPath()
	if host == "" || os.Getenv("DYNAPP_APP_HOST") != "" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	// apt installs both into /usr/bin, which apt also updates.
	if filepath.Dir(host) != filepath.Dir(executable) || unix.Access(filepath.Dir(host), unix.W_OK) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	want := normalizedReleaseVersion(config.Version)
	installed := nativeHostVersion(ctx, host)
	if installed == want {
		return
	}
	log.Printf("DynApp Shell agent: updating the app host from %q to %s", installed, want)
	if err := replaceNativeHostBinary(ctx, config, host, want); err != nil {
		log.Printf("DynApp Shell agent: app host update failed: %v", err)
		return
	}
	nativeSupportCache.Lock()
	nativeSupportCache.at = time.Time{}
	nativeSupportCache.Unlock()
	log.Printf("DynApp Shell agent: app host updated to %s", want)
}

func nativeHostVersion(ctx context.Context, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, host, "--version").Output()
	if err != nil {
		return ""
	}
	return normalizedReleaseVersion(strings.TrimSpace(string(output)))
}

func nativeHostAssetBase(version string) string {
	return fmt.Sprintf("%s-%s-linux-%s", nativeHostExecutable, version, runtime.GOARCH)
}

// replaceNativeHostBinary downloads the release's host with the same
// checksum and Ed25519 signature checks as agent updates, then renames it
// over the installed host. Running apps keep the old file until they quit.
func replaceNativeHostBinary(ctx context.Context, config SelfUpdateConfig, host, version string) error {
	release, err := fetchRelease(ctx, config, "tags/v"+version)
	if err != nil {
		return err
	}
	base := nativeHostAssetBase(version)
	var binary, checksum, signature githubAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case base:
			binary = asset
		case base + ".sha256":
			checksum = asset
		case base + ".sig":
			signature = asset
		}
	}
	if binary.BrowserURL == "" || checksum.BrowserURL == "" || signature.BrowserURL == "" {
		return fmt.Errorf("release v%s has no signed %s", version, base)
	}
	if binary.Size <= 0 || binary.Size > maxAgentBinaryBytes {
		return errors.New("app host size is invalid")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	expected, err := downloadChecksum(ctx, client, checksum.BrowserURL)
	if err != nil {
		return err
	}
	sig, err := downloadSignature(ctx, client, signature.BrowserURL)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(host), "."+nativeHostExecutable+"-*.download")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := downloadAsset(ctx, client, binary.BrowserURL, temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return errors.New("app host checksum mismatch")
	}
	if err := verifyReleaseSignature(actual, sig); err != nil {
		return err
	}
	return os.Rename(path, host)
}
