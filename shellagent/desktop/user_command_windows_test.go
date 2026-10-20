//go:build windows

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
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

// Model npm/CLI startup: our hidden launcher starts cmd.exe, which starts
// another console program without any hide flags of its own.
func TestUserCommandDescendantsStayHidden(t *testing.T) {
	const marker = "DYNAPP_TEST_CONSOLE_CHILD"
	switch os.Getenv(marker) {
	case "launcher":
		child := exec.Command("cmd.exe", "/d", "/c", os.Args[0], "-test.run=^TestUserCommandDescendantsStayHidden$")
		child.Env = append(os.Environ(), marker+"=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "child":
		console, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		visible := uintptr(0)
		if console != 0 {
			visible, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible").Call(console)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"console": console, "visible": visible != 0})
		os.Exit(0)
	}
	user := &UserEnvironment{}
	command := user.Command(context.Background(), os.Args[0], "-test.run=^TestUserCommandDescendantsStayHidden$")
	command.Env = append(os.Environ(), marker+"=launcher")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("descendant probe: %v: %s", err, output)
	}
	var probe struct {
		Console uintptr
		Visible bool
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatalf("probe output: %v: %s", err, output)
	}
	t.Logf("descendant console: %s", output)
	if probe.Visible {
		t.Fatal("cmd.exe descendant opened a visible console")
	}
}
