package shellagent

import (
	"os"
	"path/filepath"
	"strings"

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
