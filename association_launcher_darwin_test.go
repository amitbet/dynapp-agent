//go:build darwin

package shellagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDarwinAssociationLauncherForwardsFiles(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "arguments")
	executable := filepath.Join(root, `agent ' " $ name`)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > "+shellQuote(output)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := AssociationOptions{AppID: "viewer", Executable: executable, URL: "https://example.com/app?name=a&value='quoted'"}
	files := []string{filepath.Join(root, `first ' $ file.md`), filepath.Join(root, `second " file.md`)}
	literals := []string{}
	for _, path := range files {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		literals = append(literals, "POSIX file "+strconv.Quote(path))
	}
	script := strings.Replace(darwinAssociationLauncher(options), "my launchFiles({})", "my launchFiles({"+strings.Join(literals, ", ")+"})", 1)
	if data, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v: %s", err, data)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := append([]string{"--app-id", options.AppID, "--app-url", options.URL, "launch-app"}, files...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}
