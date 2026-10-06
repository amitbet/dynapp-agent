//go:build windows

package main

import (
	"context"
	"debug/pe"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsConsoleModes(t *testing.T) {
	for _, args := range [][]string{
		{"app", "--id", "owner/app", "--", `C:\folder with spaces\book.xlsx`},
		{"--app-id", "notes", "--app-url", "https://example.com", "launch-app", `C:\notes.txt`},
		{"open-url", "dynapp://start"}, {"--background"}, {"-background=true"},
		{"--update-helper", "--update-source", "download.exe"},
		{"presentation-helper", "session"}, {"file-promise-helper"},
	} {
		if needsParentConsole(args) {
			t.Errorf("desktop launch would attach a console: %q", args)
		}
	}
	for _, args := range [][]string{
		nil, {"install-user"}, {"install"}, {"--version"}, {"--help"},
		{"app", "--help"}, {"--background=false"}, {"mcp-proxy"},
	} {
		if !needsParentConsole(args) {
			t.Errorf("CLI launch would lose its console: %q", args)
		}
	}
}

func TestWindowsGUIExecutablePreservesCLIOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "dynapp-shell-agent.exe")
	build := exec.CommandContext(ctx, "go", "build", "-ldflags=-H=windowsgui", "-o", path, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Windows desktop executable: %v: %s", err, output)
	}
	file, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var subsystem uint16
	switch header := file.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		subsystem = header.Subsystem
	case *pe.OptionalHeader32:
		subsystem = header.Subsystem
	default:
		t.Fatal("Windows executable has no PE optional header")
	}
	if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		t.Fatalf("desktop executable would allocate a console: subsystem %d", subsystem)
	}
	for _, args := range [][]string{{"--version"}, {"--help"}, {"app", "--help"}} {
		command := exec.CommandContext(ctx, path, args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %q failed: %v: %s", args, err, output)
		}
		if len(args) == 1 && args[0] == "--version" {
			if strings.TrimSpace(string(output)) != "dev" {
				t.Fatalf("redirected version output = %q", output)
			}
		} else if !strings.Contains(string(output), "Usage") {
			t.Fatalf("redirected help output = %q", output)
		}
	}
}
