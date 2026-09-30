package desktop

import (
	"os"
	"strings"
)

// UserEnvironment describes the signed-in desktop user a command should run
// as. The Windows service runs as LocalSystem, whose PATH, profile, and CLI
// sign-ins are not the user's; coding-agent CLIs must run as the user.
type UserEnvironment struct {
	env     []string
	release func()
	token   userToken
	// Impersonated reports that commands run as a different account than the
	// agent, so agent-owned directories may not be readable by them.
	Impersonated bool
}

// Environ returns the environment commands receive.
func (u *UserEnvironment) Environ() []string {
	if u.env != nil {
		return append([]string(nil), u.env...)
	}
	return os.Environ()
}

// Getenv reads one variable from the user's environment.
func (u *UserEnvironment) Getenv(key string) string {
	if u.env == nil {
		return os.Getenv(key)
	}
	return envValue(u.env, key)
}

// Close releases the user's token. Call it once every command has started.
func (u *UserEnvironment) Close() {
	if u.release != nil {
		u.release()
		u.release = nil
	}
}

func envValue(env []string, key string) string {
	for index := len(env) - 1; index >= 0; index-- {
		name, value, ok := strings.Cut(env[index], "=")
		if ok && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}
