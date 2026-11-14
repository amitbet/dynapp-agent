//go:build windows

package shellagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/amitbet/dynapp-agent/shellagent/desktop"
)

// The Windows agent usually runs as LocalSystem, so the CLI is found and
// replaced as the signed-in user: Dyner's install.ps1 runs with that user's
// token, keeps the user as the file's owner and checks the SHA-256 itself.

func findDynappCLIs() ([]cliInstall, func()) {
	user, err := desktop.CurrentUser()
	if err != nil {
		return nil, func() {}
	}
	command := func(ctx context.Context, path string, args ...string) *exec.Cmd {
		return user.Command(ctx, path, args...)
	}
	var candidates []string
	if local := user.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, "Programs", "dynapp", "dynapp.exe"))
	}
	if found, err := user.LookPath("dynapp"); err == nil && strings.EqualFold(filepath.Ext(found), ".exe") {
		candidates = append(candidates, found)
	}
	seen := map[string]bool{}
	var installs []cliInstall
	for _, candidate := range candidates {
		key := strings.ToLower(filepath.Clean(candidate))
		if seen[key] {
			continue
		}
		if info, err := os.Stat(candidate); err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[key] = true
		installs = append(installs, cliInstall{Path: candidate, command: command})
	}
	return installs, func() { user.Close() }
}

func replaceDynappCLI(ctx context.Context, base, version string, install cliInstall) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; irm '%s/install.ps1' | iex", strings.TrimRight(base, "/"))
	command := install.command(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if command.Env == nil {
		command.Env = os.Environ()
	}
	command.Env = append(command.Env, "DYNAPP_INSTALL_DIR="+filepath.Dir(install.Path), "DYNAPP_DOWNLOAD_BASE="+strings.TrimRight(base, "/"))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("install.ps1: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if got := installedCLIVersion(ctx, install, install.Path); got != version {
		return fmt.Errorf("after install.ps1 the CLI reports %q, want %s", got, version)
	}
	return nil
}
