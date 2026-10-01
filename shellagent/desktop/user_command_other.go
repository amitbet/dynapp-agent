//go:build !windows

package desktop

import (
	"context"
	"os/exec"
)

// CurrentUser returns the agent's own environment; the macOS LaunchAgent and
// Linux daemon already run as the desktop user.
func CurrentUser() (*UserEnvironment, error) { return &UserEnvironment{}, nil }

func (u *UserEnvironment) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Command builds a command that runs as the user. A nil ctx means no
// cancellation.
func (u *UserEnvironment) Command(ctx context.Context, path string, args ...string) *exec.Cmd {
	if ctx == nil {
		return exec.Command(path, args...)
	}
	return exec.CommandContext(ctx, path, args...)
}

type userToken = struct{}

// StartGUI starts a windowed program as the user.
func (u *UserEnvironment) StartGUI(path string, args []string, dir string) error {
	command := exec.Command(path, args...)
	command.Dir = dir
	return command.Start()
}
