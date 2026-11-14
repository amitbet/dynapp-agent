//go:build !windows && !android

package shellagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// findDynappCLIs is the installer's default (~/.local/bin/dynapp) and the
// dynapp on PATH, each once. Only regular, writable files count.
func findDynappCLIs() ([]cliInstall, func()) {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "dynapp"))
	}
	if found, err := exec.LookPath("dynapp"); err == nil {
		candidates = append(candidates, found)
	}
	return uniqueCLIInstalls(candidates, func(ctx context.Context, path string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, path, args...)
	}), func() {}
}

func uniqueCLIInstalls(candidates []string, command func(context.Context, string, ...string) *exec.Cmd) []cliInstall {
	seen := map[string]bool{}
	var installs []cliInstall
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || seen[resolved] {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[resolved] = true
		installs = append(installs, cliInstall{Path: resolved, command: command})
	}
	return installs
}

// replaceDynappCLI downloads the release beside the old binary, checks its
// SHA-256 sidecar and that it runs and reports version, then renames it over
// the old one, so a failure leaves the old CLI in place.
func replaceDynappCLI(ctx context.Context, base, version string, install cliInstall) error {
	target := runtime.GOOS + "-" + runtime.GOARCH
	url := strings.TrimRight(base, "/") + "/download/cli/" + target
	binary, err := cliGet(ctx, url, maxCLIBinarySize)
	if err != nil {
		return err
	}
	sidecar, err := cliGet(ctx, url+".sha256", 1024)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(sidecar))
	sum := sha256.Sum256(binary)
	if len(fields) == 0 || !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
		return errors.New("downloaded binary does not match its .sha256")
	}
	staged, err := os.CreateTemp(filepath.Dir(install.Path), ".dynapp-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if _, err := staged.Write(binary); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Chmod(staged.Name(), 0o755); err != nil {
		return err
	}
	if got := installedCLIVersion(ctx, install, staged.Name()); got != version {
		return fmt.Errorf("downloaded binary reports version %q, want %s", got, version)
	}
	return os.Rename(staged.Name(), install.Path)
}
