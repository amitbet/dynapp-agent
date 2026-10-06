//go:build windows

package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestUserCommandHidesConsole(t *testing.T) {
	for _, impersonated := range []bool{false, true} {
		for _, ctx := range []context.Context{nil, context.Background()} {
			user := &UserEnvironment{}
			if impersonated {
				user.env = []string{"DYNAPP_TEST_USER=desktop"}
				user.token = userToken(123)
			}
			command := user.Command(ctx, "powershell.exe", "-NoProfile", "-NonInteractive")
			attrs := command.SysProcAttr
			if attrs == nil || !attrs.HideWindow || attrs.CreationFlags&createNoWindow == 0 {
				t.Fatalf("impersonated=%v ctx=%v: console is not suppressed: %+v", impersonated, ctx, attrs)
			}
			if attrs.Token != syscall.Token(user.token) {
				t.Fatalf("impersonated=%v: token = %v", impersonated, attrs.Token)
			}
			if impersonated && envValue(command.Env, "DYNAPP_TEST_USER") != "desktop" {
				t.Fatal("desktop user's environment was lost")
			}
		}
	}
}

func TestUserCommandPowerShellHasNoConsole(t *testing.T) {
	path := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	const script = `Add-Type -TypeDefinition 'using System; using System.Runtime.InteropServices; public static class ConsoleProbe { [DllImport("kernel32.dll")] public static extern IntPtr GetConsoleWindow(); }'; [ConsoleProbe]::GetConsoleWindow().ToInt64()`
	user := &UserEnvironment{}
	output, err := user.Command(context.Background(), path, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell helper failed: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != "0" {
		t.Fatalf("PowerShell allocated a console: %s", output)
	}
}
