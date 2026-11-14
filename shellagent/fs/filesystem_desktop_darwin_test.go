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

func TestMacTrashMovesDisposableFile(t *testing.T) {
	if os.Getenv("DYNAPP_TEST_FINDER_TRASH") != "1" {
		t.Skip("set DYNAPP_TEST_FINDER_TRASH=1 to move a disposable file to Finder Trash")
	}
	path := filepath.Join(t.TempDir(), "trash check 'quotes'.txt")
	if err := os.WriteFile(path, []byte("disposable trash test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := trashPath(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists after trash: %v", err)
	}
}

func TestMacTrashSendsBatchInOneFinderCall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	// Record each osascript invocation instead of touching the real Trash.
	script := "#!/bin/sh\necho \"$#\" >> '" + logPath + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var paths []string
	for _, name := range []string{"a.txt", "b c.txt", "d.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if _, err := Call("trash", []any{[]any{paths[0], paths[1], paths[2]}}, nil); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// One call: -e, script, --, and the three paths.
	if got := strings.TrimSpace(string(calls)); got != "6" {
		t.Fatalf("expected one osascript call with 6 args, got %q", got)
	}
}

func TestTrashRejectsInvalidBatch(t *testing.T) {
	for _, args := range [][]any{{[]any{}}, {[]any{"/tmp", 3}}, {[]any{""}}} {
		if _, err := Call("trash", args, nil); err == nil {
			t.Fatalf("expected %v to be rejected", args)
		}
	}
}
