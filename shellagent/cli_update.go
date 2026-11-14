package shellagent

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The dynapp developer CLI (https://dynapp.io/install.sh) has no updater of
// its own, and coding agents keep calling whatever copy the user installed
// once. The agent keeps an existing install current: it never installs the
// CLI where there is none, and it leaves development builds ("dynapp dev")
// alone. The platform files find the installs and replace one
// (cli_update_unix.go, cli_update_windows.go).

const (
	cliUpdateEvery   = 6 * time.Hour
	cliCheckTimeout  = 2 * time.Minute
	maxCLIBinarySize = 128 << 20
)

// startCLIUpdater checks at startup and then every few hours until ctx ends.
func (s *Server) startCLIUpdater(ctx context.Context) {
	go func() {
		s.checkCLIUpdate(ctx)
		ticker := time.NewTicker(cliUpdateEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkCLIUpdate(ctx)
			}
		}
	}()
}

func (s *Server) checkCLIUpdate(ctx context.Context) {
	s.mu.Lock()
	base := s.Config.dynerBaseOrigin()
	s.mu.Unlock()
	if base == "" {
		base = defaultNoteDynerURL
	}
	ctx, cancel := context.WithTimeout(ctx, cliCheckTimeout)
	defer cancel()
	for _, update := range updateDynappCLIs(ctx, base) {
		if update.Err != nil {
			log.Printf("dynapp CLI update: %s %s: %v", update.Path, update.From, update.Err)
		} else if update.To != "" {
			log.Printf("dynapp CLI update: %s %s -> %s", update.Path, update.From, update.To)
		}
	}
}

type cliUpdate struct {
	Path, From, To string
	Err            error
}

// cliInstall is one dynapp binary on this machine and how to run it.
type cliInstall struct {
	Path    string
	command func(ctx context.Context, path string, args ...string) *exec.Cmd
}

// updateDynappCLIs brings every installed release CLI up to the version
// Dyner mirrors at base/download/cli/version.
func updateDynappCLIs(ctx context.Context, base string) []cliUpdate {
	installs, release := findDynappCLIs()
	defer release()
	return updateCLIInstalls(ctx, base, installs)
}

func updateCLIInstalls(ctx context.Context, base string, installs []cliInstall) []cliUpdate {
	if len(installs) == 0 {
		return nil
	}
	latest, err := latestCLIVersion(ctx, base)
	if err != nil {
		return []cliUpdate{{Path: "dynapp", Err: fmt.Errorf("version check: %w", err)}}
	}
	var updates []cliUpdate
	for _, install := range installs {
		current := installedCLIVersion(ctx, install, install.Path)
		if !newerRelease(latest, current) {
			continue
		}
		update := cliUpdate{Path: install.Path, From: current}
		if err := replaceDynappCLI(ctx, base, latest, install); err != nil {
			update.Err = err
		} else {
			update.To = latest
		}
		updates = append(updates, update)
	}
	return updates
}

func latestCLIVersion(ctx context.Context, base string) (string, error) {
	body, err := cliGet(ctx, strings.TrimRight(base, "/")+"/download/cli/version", 64)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if !isReleaseVersion(version) {
		return "", fmt.Errorf("unexpected version %q", version)
	}
	return normalizedReleaseVersion(version), nil
}

// installedCLIVersion is the X.Y.Z that `dynapp version` prints, or "" for a
// development build or anything that is not the dynapp CLI.
func installedCLIVersion(ctx context.Context, install cliInstall, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := install.command(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	name, version, ok := strings.Cut(strings.TrimSpace(string(output)), " ")
	if !ok || name != "dynapp" || !isReleaseVersion(version) {
		return ""
	}
	return normalizedReleaseVersion(version)
}

func cliGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response is larger than %d bytes", url, limit)
	}
	return body, nil
}

// isElectronAppDir reports the folder of the Electron DynApp.exe, which on a
// case-insensitive file system answers to dynapp.exe. Running it with
// "version" would start that app, so it is never treated as the CLI.
func isElectronAppDir(dir string) bool {
	for _, marker := range []string{filepath.Join("resources", "app.asar"), "resources.pak", "LICENSES.chromium.html"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}
