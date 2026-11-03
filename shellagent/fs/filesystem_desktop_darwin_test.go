package fs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacTrashScriptCompiles(t *testing.T) {
	// Compile the production script without executing it or deleting any files.
	cmd := exec.Command("/usr/bin/osacompile", "-o", filepath.Join(t.TempDir(), "trash.scpt"))
	cmd.Stdin = strings.NewReader(macTrashScript)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile trash AppleScript: %v: %s", err, output)
	}
}

func TestMacTrashReportsScriptError(t *testing.T) {
	dir := t.TempDir()
	// Substitute osascript so this test cannot move anything to the Trash.
	if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte("#!/bin/sh\nprintf '%s\\n' 'Finder denied access' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := trashPath(path); err == nil || !strings.Contains(err.Error(), "Finder denied access") {
		t.Fatalf("expected script error details, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("test file was changed: %v", err)
	}
}
