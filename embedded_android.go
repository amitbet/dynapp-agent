//go:build android

package main

import (
	"io"
	"os"
)

// The Android app ships the agent inside its APK and updates it with the APK.
const selfUpdateSupported = false

// The app starts the agent with a pipe on stdin and never writes to it. When
// the app process dies the pipe closes, and the agent must not outlive it.
func init() {
	if os.Getenv("DYNAPP_EXIT_WITH_STDIN") != "1" {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
}
