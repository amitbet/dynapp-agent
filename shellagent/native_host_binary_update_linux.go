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
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The agent itself downloads and updates dynapp-app-host, like it updates
// itself: a signed release asset, checked, then renamed into place. install.sh
// only installs the agent, because endpoint protection treats a downloaded
// script that fetches more programs as a dropper (the Windows installer was
// changed for the same reason). Package managers own hosts elsewhere
// (/usr/bin) and update them themselves.

// nativeAgentHostDir is where the agent puts a host when the folder it runs
// from is not writable (install.sh as root puts the agent in /usr/local/bin).
func nativeAgentHostDir() string {
	data, err := xdgDataHome()
	if err != nil {
		return ""
	}
	return filepath.Join(data, "dynapp", "bin")
}

func agentExecutableDir() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Dir(executable)
}

func dirWritable(dir string) bool { return dir != "" && unix.Access(dir, unix.W_OK) == nil }

// nativeHostInstallTarget is where a missing host is downloaded to: next to
// the agent when that folder is writable, otherwise nativeAgentHostDir.
func nativeHostInstallTarget() string {
	if dir := agentExecutableDir(); dirWritable(dir) {
		return filepath.Join(dir, nativeHostExecutable)
	}
	if dir := nativeAgentHostDir(); dir != "" {
		return filepath.Join(dir, nativeHostExecutable)
	}
	return ""
}

// nativeHostManaged reports whether the agent owns this host: it sits next to
// the agent or in nativeAgentHostDir, and the agent may replace it.
func nativeHostManaged(host string) bool {
	dir := filepath.Dir(host)
	if dir != agentExecutableDir() && dir != nativeAgentHostDir() {
		return false
	}
	return dirWritable(dir)
}

var nativeHostFetch struct {
	sync.Mutex
	running     bool
	lastFailure time.Time
}

// nativeHostRetryAfter spaces out failed downloads.
const nativeHostRetryAfter = time.Hour

// ensureNativeHost starts a download when the host is missing, the user is
// in a desktop session, and the agent may fetch it. It reports whether a
// download is running, so the caller can say so instead of "install it".
func (s *Server) ensureNativeHost() bool {
	if nativeHostPath() != "" || !s.nativeHostFetchAllowed() {
		return false
	}
	environment := nativeSessionEnv()
	if environmentValue(environment, "DISPLAY") == "" && environmentValue(environment, "WAYLAND_DISPLAY") == "" {
		return false
	}
	nativeHostFetch.Lock()
	defer nativeHostFetch.Unlock()
	if nativeHostFetch.running {
		return true
	}
	if !nativeHostFetch.lastFailure.IsZero() && time.Since(nativeHostFetch.lastFailure) < nativeHostRetryAfter {
		return false
	}
	nativeHostFetch.running = true
	go s.runNativeHostFetch()
	return true
}

func (s *Server) runNativeHostFetch() {
	err := s.maintainNativeHost()
	nativeHostFetch.Lock()
	nativeHostFetch.running = false
	if err != nil {
		nativeHostFetch.lastFailure = time.Now()
	} else {
		nativeHostFetch.lastFailure = time.Time{}
	}
	nativeHostFetch.Unlock()
}

// updateNativeHostBinary runs at agent start: it updates a host the agent
// owns, or downloads one on a desktop that has none.
func (s *Server) updateNativeHostBinary() {
	if nativeHostPath() == "" {
		s.ensureNativeHost()
		return
	}
	nativeHostFetch.Lock()
	if nativeHostFetch.running {
		nativeHostFetch.Unlock()
		return
	}
	nativeHostFetch.running = true
	nativeHostFetch.Unlock()
	s.runNativeHostFetch()
}

// nativeHostFetchAllowed: release builds with self-update on, native apps
// not turned off, and no host path chosen by hand.
func (s *Server) nativeHostFetchAllowed() bool {
	s.mu.Lock()
	config := s.selfUpdateConfig
	disabled := s.Config.NativeAppsDisabled
	s.mu.Unlock()
	if !config.Enabled || !isReleaseVersion(config.Version) || disabled || EnvDisabled("DYNAPP_NATIVE_APPS") {
		return false
	}
	return !filepath.IsAbs(os.Getenv("DYNAPP_APP_HOST"))
}

// maintainNativeHost downloads a missing host or brings an owned one to the
// agent's release.
func (s *Server) maintainNativeHost() error {
	if !s.nativeHostFetchAllowed() {
		return nil
	}
	s.mu.Lock()
	config := s.selfUpdateConfig
	s.mu.Unlock()
	want := normalizedReleaseVersion(config.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	host := nativeHostPath()
	if host == "" {
		target := nativeHostInstallTarget()
		if target == "" {
			return errors.New("no folder for the app host")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			log.Printf("DynApp Shell agent: app host download failed: %v", err)
			return err
		}
		log.Printf("DynApp Shell agent: downloading the app host %s to %s", want, target)
		if err := replaceNativeHostBinary(ctx, config, target, want); err != nil {
			log.Printf("DynApp Shell agent: app host download failed: %v", err)
			return err
		}
		resetNativeSupportCache()
		log.Printf("DynApp Shell agent: app host %s installed", want)
		return nil
	}
	if !nativeHostManaged(host) {
		return nil
	}
	installed := nativeHostVersion(ctx, host)
	if installed == want {
		return nil
	}
	log.Printf("DynApp Shell agent: updating the app host from %q to %s", installed, want)
	if err := replaceNativeHostBinary(ctx, config, host, want); err != nil {
		log.Printf("DynApp Shell agent: app host update failed: %v", err)
		return err
	}
	resetNativeSupportCache()
	log.Printf("DynApp Shell agent: app host updated to %s", want)
	return nil
}

func resetNativeSupportCache() {
	nativeSupportCache.Lock()
	nativeSupportCache.at = time.Time{}
	nativeSupportCache.Unlock()
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
