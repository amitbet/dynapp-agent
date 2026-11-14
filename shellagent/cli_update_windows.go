//go:build windows

package shellagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/amitbet/dynapp-agent/shellagent/desktop"
)

// The Windows agent usually runs as LocalSystem, so the CLI is found and its
// version read as the signed-in user. The download itself is written by the
// agent (cli_update_replace.go); it inherits the install folder's ACL, which
// gives that user full control.

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
		candidates = append(candidates,
			filepath.Join(local, "Programs", "dynapp-cli", "dynapp.exe"),
			filepath.Join(local, "Programs", "dynapp", "dynapp.exe"))
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
		if info, err := os.Stat(candidate); err != nil || !info.Mode().IsRegular() || isElectronAppDir(filepath.Dir(candidate)) {
			continue
		}
		seen[key] = true
		installs = append(installs, cliInstall{Path: candidate, command: command})
	}
	return installs, func() { user.Close() }
}

// installCLIBinary puts the checked download in place of the old CLI. A
// running dynapp.exe cannot be replaced but can be renamed, so the old binary
// moves aside first and comes back if the new one cannot take its place. A
// leftover aside copy is removed on the next update.
func installCLIBinary(staged, target string) error {
	aside := target + ".old"
	_ = os.Remove(aside)
	if err := os.Rename(target, aside); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(aside, target)
		return err
	}
	_ = os.Remove(aside)
	return nil
}
