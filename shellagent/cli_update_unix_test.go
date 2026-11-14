//go:build !windows && !android

package shellagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func fakeCLI(version string) []byte { return []byte("#!/bin/sh\necho dynapp " + version + "\n") }

func cliServer(t *testing.T, latest string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	target := "/download/cli/" + runtime.GOOS + "-" + runtime.GOARCH
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/cli/version":
			_, _ = w.Write([]byte(latest + "\n"))
		case target:
			_, _ = w.Write(binary)
		case target + ".sha256":
			_, _ = w.Write([]byte(sum + "  dynapp-x\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeCLI(t *testing.T, version string) cliInstall {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dynapp")
	if err := os.WriteFile(path, fakeCLI(version), 0o755); err != nil {
		t.Fatal(err)
	}
	installs := uniqueCLIInstalls([]string{path, path}, func(ctx context.Context, path string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, path, args...)
	})
	if len(installs) != 1 {
		t.Fatalf("installs: %+v", installs)
	}
	return installs[0]
}

func TestCLIUpdateReplacesAnOlderRelease(t *testing.T) {
	binary := fakeCLI("0.2.0")
	sum := sha256.Sum256(binary)
	server := cliServer(t, "0.2.0", binary, hex.EncodeToString(sum[:]))
	install := writeCLI(t, "0.1.9")
	updates := updateCLIInstalls(context.Background(), server.URL, []cliInstall{install})
	if len(updates) != 1 || updates[0].Err != nil || updates[0].From != "0.1.9" || updates[0].To != "0.2.0" {
		t.Fatalf("updates: %+v", updates)
	}
	if got := installedCLIVersion(context.Background(), install, install.Path); got != "0.2.0" {
		t.Fatalf("installed %q", got)
	}
	if again := updateCLIInstalls(context.Background(), server.URL, []cliInstall{install}); len(again) != 0 {
		t.Fatalf("a current CLI must be left alone: %+v", again)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(install.Path), ".dynapp-update-*"))
	if len(leftovers) != 0 {
		t.Fatalf("staging files left behind: %v", leftovers)
	}
}

func TestCLIUpdateKeepsTheOldBinaryOnABadDownload(t *testing.T) {
	server := cliServer(t, "0.2.0", fakeCLI("0.2.0"), "0000000000000000000000000000000000000000000000000000000000000000")
	install := writeCLI(t, "0.1.9")
	updates := updateCLIInstalls(context.Background(), server.URL, []cliInstall{install})
	if len(updates) != 1 || updates[0].Err == nil {
		t.Fatalf("a checksum mismatch must fail: %+v", updates)
	}
	if got := installedCLIVersion(context.Background(), install, install.Path); got != "0.1.9" {
		t.Fatalf("the old CLI must stay: %q", got)
	}

	wrong := fakeCLI("0.1.5")
	sum := sha256.Sum256(wrong)
	mislabeled := cliServer(t, "0.2.0", wrong, hex.EncodeToString(sum[:]))
	if updates := updateCLIInstalls(context.Background(), mislabeled.URL, []cliInstall{install}); len(updates) != 1 || updates[0].Err == nil {
		t.Fatalf("a binary that reports another version must be rejected: %+v", updates)
	}
}

func TestCLIUpdateLeavesDevelopmentBuildsAlone(t *testing.T) {
	binary := fakeCLI("0.2.0")
	sum := sha256.Sum256(binary)
	server := cliServer(t, "0.2.0", binary, hex.EncodeToString(sum[:]))
	install := writeCLI(t, "dev")
	if updates := updateCLIInstalls(context.Background(), server.URL, []cliInstall{install}); len(updates) != 0 {
		t.Fatalf("a dev build must not be replaced: %+v", updates)
	}
}

func TestElectronAppFolderIsNotTheCLI(t *testing.T) {
	dir := t.TempDir()
	if isElectronAppDir(dir) {
		t.Fatal("an empty folder is not an Electron app")
	}
	_ = os.WriteFile(filepath.Join(dir, "LICENSES.chromium.html"), nil, 0o644)
	if !isElectronAppDir(dir) {
		t.Fatal("an Electron distribution must be recognized")
	}
}
