//go:build windows

package main

import (
	"os"
	"os/exec"
	"testing"
)

func blockedHelper() *exec.Cmd {
	return exec.Command(os.Getenv("SystemRoot")+`\System32\cmd.exe`, "/c", "exit 3")
}

func TestURLHandlerCommandQuotesTheExecutable(t *testing.T) {
	if got := urlHandlerCommand(`C:\Program Files\DynApp\dynapp-shell-agent.exe`); got != `"C:\Program Files\DynApp\dynapp-shell-agent.exe" open-url "%1"` {
		t.Fatalf("command = %s", got)
	}
}
