//go:build !windows

package main

import (
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
	"syscall"
	"testing"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
)

// Exercise a real listener shutdown, executable replacement, and syscall.Exec
// in a disposable process. Unit tests with an unstarted server miss the race.
func TestUnixSelfUpdateProcess(t *testing.T) {
	if dir := os.Getenv("DYNAPP_TEST_UNIX_UPDATE_DIR"); dir != "" {
		runUnixUpdateProcess(t, dir)
		return
	}
	for _, baseline := range []bool{true, false} {
		t.Run(fmt.Sprintf("releasedMain=%t", baseline), func(t *testing.T) {
			dir := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "agent")
			for _, path := range []string{target, filepath.Join(dir, "download")} {
				if err := copyFile(executable, path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, target, "-test.run=^TestUnixSelfUpdateProcess$", "-test.v")
			command.Env = append(os.Environ(), "DYNAPP_TEST_UNIX_UPDATE_DIR="+dir,
				"DYNAPP_TEST_UNIX_ADDRESS="+address, "DYNAPP_TEST_UNIX_BASELINE="+strconv.FormatBool(baseline))
			output, err := command.CombinedOutput()
			if baseline {
				// Reproduce the released foreground main path, which calls
				// ListenAndServe directly and exits on listener shutdown.
				if command.ProcessState == nil || command.ProcessState.ExitCode() != 1 {
					t.Fatalf("released main did not reproduce premature exit: %v\n%s", err, output)
				}
				t.Logf("reproduced released foreground shutdown race:\n%s", output)
				return
			}
			if err != nil {
				t.Fatalf("foreground update failed: %v\n%s", err, output)
			}
			after, err := os.Stat(target)
			if err != nil || os.SameFile(before, after) {
				t.Fatalf("installed executable was not replaced: %v", err)
			}
			pid, err := os.ReadFile(filepath.Join(dir, "healthy.pid"))
			if err != nil || string(pid) != strconv.Itoa(command.ProcessState.Pid()) {
				t.Fatalf("replacement did not exec and serve health in the original PID: %q, %v\n%s", pid, err, output)
			}
		})
	}
}

func runUnixUpdateProcess(t *testing.T, dir string) {
	address := os.Getenv("DYNAPP_TEST_UNIX_ADDRESS")
	p := &program{server: &shellagent.Server{Address: address, StateDir: filepath.Join(dir, "state")}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := waitForUnixTestAgent(ctx, address); err != nil {
			log.Fatal(err)
		}
		if os.Getenv("DYNAPP_TEST_UNIX_EXECED") == "1" {
			if err := os.WriteFile(filepath.Join(dir, "healthy.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				log.Fatal(err)
			}
			os.Exit(0)
		}
		// Give the main goroutine a deterministic scheduling opportunity after
		// the listener closes. Then perform the actual platform exec syscall.
		execve = func(path string, args, env []string) error {
			time.Sleep(100 * time.Millisecond)
			return syscall.Exec(path, args, append(env, "DYNAPP_TEST_UNIX_EXECED=1"))
		}
		if err := p.scheduleSelfUpdate(ctx, filepath.Join(dir, "download"), "test-new"); err != nil {
			log.Fatal(err)
		}
	}()
	var err error
	if os.Getenv("DYNAPP_TEST_UNIX_BASELINE") == "true" {
		err = p.server.ListenAndServe()
	} else {
		err = p.listenAndServe()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func waitForUnixTestAgent(ctx context.Context, address string) error {
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
