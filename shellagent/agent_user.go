package shellagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/desktop"
)

// agentUser is the account coding-agent CLIs run as: the signed-in desktop
// user, whose PATH and CLI sign-ins they need. Without an active desktop
// session the agent's own account is used. Close it after Start.
func agentUser() *desktop.UserEnvironment {
	user, err := desktop.CurrentUser()
	if err != nil {
		return &desktop.UserEnvironment{}
	}
	return user
}

// agentBinary resolves a CLI from its override variable or the user's PATH.
func agentBinary(user *desktop.UserEnvironment, envName, binary string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		return path, nil
	}
	return user.LookPath(binary)
}

// agentWorkspace is the per-app working directory for an agent session. A
// CLI running as another account can't use the service's state directory, so
// it gets one under that user's local app data.
func (a *agentService) agentWorkspace(user *desktop.UserEnvironment, appID string) (string, error) {
	root := ""
	if user.Impersonated {
		if local := user.Getenv("LOCALAPPDATA"); local != "" {
			root = filepath.Join(local, "DynApp")
		}
	}
	if root == "" {
		root = a.server.StateDir
	}
	if root == "" {
		root, _ = DefaultStateDir()
	}
	cwd := filepath.Join(root, "agent-workspaces", safeName(appID))
	return cwd, os.MkdirAll(cwd, 0o700)
}

// codexEnvironment adds the configured CODEX_HOME, if any.
func codexEnvironment(user *desktop.UserEnvironment) []string {
	env := user.Environ()
	if home := strings.TrimSpace(os.Getenv("DYNAPP_AGENT_HOME")); home != "" {
		env = append(env, "CODEX_HOME="+home)
	}
	return env
}

type noDaemonKey struct {
	path     string
	modified time.Time
}

var codexNoDaemon sync.Map // noDaemonKey -> bool

// codexArgs prefixes --no-daemon when this Codex supports it. The shared
// app-server daemon refuses to start for a Windows administrator ("start the
// Windows daemon from a non-elevated terminal"); DynApp talks to its own
// stdio app-server and never needs the daemon. Older Codex builds reject the
// flag, so it is detected from --help once per binary version.
func codexArgs(ctx context.Context, user *desktop.UserEnvironment, path string, args ...string) []string {
	if supportsCodexNoDaemon(ctx, user, path) {
		return append([]string{"--no-daemon"}, args...)
	}
	return args
}

func supportsCodexNoDaemon(ctx context.Context, user *desktop.UserEnvironment, path string) bool {
	key := noDaemonKey{path: path}
	if info, err := os.Stat(path); err == nil {
		key.modified = info.ModTime()
	}
	if known, ok := codexNoDaemon.Load(key); ok {
		return known.(bool)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	helpCtx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	command := user.Command(helpCtx, path, "--help")
	command.Env = codexEnvironment(user)
	command.WaitDelay = 2 * time.Second
	output, err := command.CombinedOutput()
	if err != nil && helpCtx.Err() != nil {
		// A timeout says nothing about the flag; ask again next time.
		return false
	}
	supported := strings.Contains(string(output), "--no-daemon")
	codexNoDaemon.Store(key, supported)
	return supported
}
