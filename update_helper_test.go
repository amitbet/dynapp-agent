package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveInstallPathFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "keg", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveInstallPath(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolveInstallPath = %q, want %q", got, want)
	}
}

func TestReplaceExecutableUpdatesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	keg := filepath.Join(dir, "Cellar", "0.1.3", "bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(keg, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o555); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "updates", "dynapp-shell-agent-0.1.5")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveInstallPath(link)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := replaceExecutable(source, resolved)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("homebrew prefix path is no longer a symlink: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("keg binary = %q, %v", data, err)
	}
	old, err := os.ReadFile(backup)
	if err != nil || string(old) != "old" {
		t.Fatalf("backup = %q, %v", old, err)
	}
}

func TestReplaceExecutableReplacesReadOnlyBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o555); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "dynapp-shell-agent.prepared-1")
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceExecutable(source, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("replaced binary = %q, %v", data, err)
	}
}

func TestRunUpdateHelperStopsServiceBeforeReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "update", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	var stopped, started bool
	previousStop, previousStart := stopUpdated, startUpdated
	stopUpdated = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "old" {
			t.Fatalf("service stopped after replace: %q, %v", data, err)
		}
		stopped = true
		return nil
	}
	startUpdated = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "new" {
			t.Fatalf("service started before replace: %q, %v", data, err)
		}
		started = true
		return nil
	}
	t.Cleanup(func() {
		stopUpdated = previousStop
		startUpdated = previousStart
	})
	t.Setenv("DYNAPP_UPDATE_SERVICE", "1")
	if err := runUpdateHelper(source, target, deadPID(t)); err != nil {
		t.Fatal(err)
	}
	if !stopped || !started {
		t.Fatalf("stopped=%v started=%v", stopped, started)
	}
}

func TestRunUpdateHelperKeepsInstallIfRestartFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "update", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := startUpdated
	startUpdated = func(string) error { return errors.New("service already loaded") }
	t.Cleanup(func() { startUpdated = previous })
	if err := runUpdateHelper(source, target, deadPID(t)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("installed binary rolled back = %q, %v", data, err)
	}
	if matches, _ := filepath.Glob(target + ".old-*"); len(matches) != 0 {
		t.Fatalf("backup left behind: %v", matches)
	}
}

func TestRunUpdateHelperDoesNotStopWhenNotAService(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynapp-shell-agent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "update", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousStop, previousStart := stopUpdated, startUpdated
	stopUpdated = func(string) error {
		t.Fatal("stop should not run outside service mode")
		return nil
	}
	startUpdated = func(string) error { return nil }
	t.Cleanup(func() {
		stopUpdated = previousStop
		startUpdated = previousStart
	})
	t.Setenv("DYNAPP_UPDATE_SERVICE", "")
	if err := runUpdateHelper(source, target, deadPID(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupStaleUpdateArtifacts(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "bin", "dynapp-shell-agent")
	updateDir := filepath.Join(dir, "updates")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []string{
		filepath.Join(updateDir, ".dynapp-update-helper-1"),
		executable + ".prepared-1",
		executable + ".old-1",
	}
	for _, path := range stale {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(updateDir, "dynapp-shell-agent-0.1.5-darwin-arm64")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupStaleUpdateArtifacts(updateDir, executable)
	for _, path := range stale {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale %s still present: %v", path, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("staged update was removed: %v", err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	command := exec.Command("true")
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd", "/c", "exit", "0")
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return command.Process.Pid
}
