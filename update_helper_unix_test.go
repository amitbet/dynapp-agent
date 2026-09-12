//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amitbet/dynapp-agent/shellagent"
)

func TestUnixSelfUpdateExecsResolvedBinary(t *testing.T) {
	dir := t.TempDir()
	keg := filepath.Join(dir, "Cellar", "bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(keg, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o555); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "updates", "new-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousExec := execve
	previousExit := osExit
	t.Cleanup(func() {
		execve = previousExec
		osExit = previousExit
	})
	var execPath string
	var execArgv []string
	execve = func(argv0 string, argv []string, _ []string) error {
		execPath = argv0
		execArgv = append([]string{}, argv...)
		return nil
	}
	osExit = func(int) {}
	p := &program{server: &shellagent.Server{}}
	if err := p.execInstalledUpdate(source, target, "0.1.5"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("keg binary = %q, %v", data, err)
	}
	if execPath != target {
		t.Fatalf("exec path = %q, want %q", execPath, target)
	}
	if len(execArgv) == 0 || execArgv[0] != target {
		t.Fatalf("exec argv = %#v", execArgv)
	}
}

func TestUnixSelfUpdateRestoresBinaryIfExecFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "updates", "new-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousExec := execve
	var exitCode int
	previousExit := osExit
	t.Cleanup(func() {
		execve = previousExec
		osExit = previousExit
	})
	execve = func(string, []string, []string) error {
		return os.ErrPermission
	}
	osExit = func(code int) { exitCode = code }
	p := &program{server: &shellagent.Server{}}
	if err := p.execInstalledUpdate(source, target, "0.1.5"); err == nil {
		t.Fatal("expected exec failure")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old" {
		t.Fatalf("binary after failed exec = %q, %v", data, err)
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
}
