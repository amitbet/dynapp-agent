//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
)

const updatedTestMarker = "\nDynApp updated process regression test\n"

// Run the real Windows replacement/restart in a disposable copy of this test
// executable. No installed agent, service, URL handler, or user state is touched.
func TestWindowsSelfUpdateProcess(t *testing.T) {
	if dir := os.Getenv("DYNAPP_TEST_UPDATE_DIR"); dir != "" {
		runWindowsUpdateProcess(t, dir)
		return
	}
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			testWindowsSelfUpdateProcess(t, rollback)
		})
	}
}

func testWindowsSelfUpdateProcess(t *testing.T, rollback bool) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "agent.exe")
	if err := copyFile(executable, oldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	newBytes := append(data, []byte(updatedTestMarker)...)
	if rollback {
		newBytes = []byte("invalid executable: exercise Windows restart failure and rollback")
	}
	if err := os.WriteFile(filepath.Join(dir, "download.exe"), newBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, oldPath, "-test.run=^TestWindowsSelfUpdateProcess$", "-test.v")
	command.Env = append(os.Environ(), "DYNAPP_TEST_UPDATE_DIR="+dir, "DYNAPP_TEST_UPDATE_ADDRESS="+address)
	// The restart uses the shipping startDetached implementation.
	output, err := command.CombinedOutput()
	t.Cleanup(func() {
		if pidText, err := os.ReadFile(filepath.Join(dir, "restarted.pid")); err == nil {
			if pid, err := strconv.Atoi(string(pidText)); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					_ = process.Kill()
					_, _ = process.Wait()
				}
			}
		}
	})
	if rollback {
		if command.ProcessState == nil || command.ProcessState.ExitCode() != 1 {
			t.Fatalf("restart failure should exit 1 after restoring the previous agent: %v\n%s", err, output)
		}
		newBytes = data
	} else if err != nil {
		t.Fatalf("original agent exited before completing update: %v\n%s", err, output)
	}
	installed, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(installed, newBytes) {
		t.Fatalf("new binary was not installed: %v\n%s", err, output)
	}
	if err := waitForTestAgent(ctx, address); err != nil {
		t.Fatalf("updated agent did not become healthy: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "restarted.pid")); err != nil {
		t.Fatalf("health endpoint was not served by the restarted binary: %v", err)
	}
}

func runWindowsUpdateProcess(t *testing.T, dir string) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	address := os.Getenv("DYNAPP_TEST_UPDATE_ADDRESS")
	p := &program{server: &shellagent.Server{Address: address, StateDir: filepath.Join(dir, "state")}}
	_, attempted := os.Stat(filepath.Join(dir, "attempted"))
	if bytes.HasSuffix(data, []byte(updatedTestMarker)) || attempted == nil {
		if err := os.WriteFile(filepath.Join(dir, "restarted.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		// Also bound the lifetime if the parent test fails before cleanup.
		go func() { time.Sleep(35 * time.Second); os.Exit(2) }()
	} else {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := waitForTestAgent(ctx, address); err != nil {
				log.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "attempted"), nil, 0o600); err != nil {
				log.Fatal(err)
			}
			if err := p.scheduleSelfUpdate(ctx, filepath.Join(dir, "download.exe"), "test-new"); err != nil {
				log.Fatal(err)
			}
		}()
	}
	// This is the foreground/background main path. Closing the listener must
	// not return here while the update goroutine still needs this process.
	if err := p.listenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func TestWindowsSelfUpdateServiceRecovery(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent.exe")
	source := filepath.Join(dir, "download.exe")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousExit := osExit
	t.Cleanup(func() { osExit = previousExit })
	exitCode := -1
	osExit = func(code int) {
		exitCode = code
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "new" {
			t.Fatalf("service exited before replacement: %q, %v", data, err)
		}
	}
	p := &program{server: &shellagent.Server{}, serviceMode: true}
	if err := p.replaceInPlaceAndRestart(source, target, "new"); err != nil {
		t.Fatal(err)
	}
	if exitCode != 1 {
		t.Fatalf("service recovery exit code = %d, want 1", exitCode)
	}
}

func waitForTestAgent(ctx context.Context, address string) error {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for health: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
