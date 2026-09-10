//go:build !windows

package desktop

import "fmt"

// Presentation helpers on macOS already run in the user LaunchAgent session.
// Windows alone needs the WTS token hand-off in user_session_windows.go.
func startUserSessionProcess(string, []string, string) error {
	return fmt.Errorf("starting a process in another user session is only needed on Windows")
}
