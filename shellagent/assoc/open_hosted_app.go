//go:build !darwin

package assoc

import (
	"os/exec"
	"runtime"
)

// OpenHostedApp opens a hosted app through the platform URL handler.
func OpenHostedApp(appURL string, _ ...string) error {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", appURL)
	} else {
		command = exec.Command("xdg-open", appURL)
	}
	return command.Start()
}
