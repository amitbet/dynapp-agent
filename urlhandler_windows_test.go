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

func TestUserPathEditsAreCaseAndSlashInsensitive(t *testing.T) {
	list := `C:\Windows;C:\Users\a\AppData\Local\Programs\DynApp\;D:\tools`
	if !pathListContains(list, `c:\users\a\appdata\local\programs\dynapp`) {
		t.Fatal("existing entry not found")
	}
	if got := pathListWithout(list, `C:\Users\a\AppData\Local\Programs\DynApp`); got != `C:\Windows;D:\tools` {
		t.Fatalf("removed = %q", got)
	}
}
