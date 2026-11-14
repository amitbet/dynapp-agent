//go:build !windows && !android

package shellagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// installCLIBinary renames the checked download over the old CLI.
func installCLIBinary(staged, target string) error {
	return os.Rename(staged, target)
}
