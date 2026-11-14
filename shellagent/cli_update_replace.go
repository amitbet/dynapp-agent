//go:build !android

package shellagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// replaceDynappCLI downloads the release beside the old binary, checks its
// SHA-256 sidecar and that it runs and reports version, then puts it in place
// of the old one, so a failure leaves the old CLI in place.
//
// The agent does this itself on every platform. It never runs a downloaded
// installer script: a background service piping a remote script into a
// shell (`irm … | iex`) is what endpoint protection (Bitdefender, Defender)
// treats as a malware download cradle, and it kills the agent for it.
func replaceDynappCLI(ctx context.Context, base, version string, install cliInstall) error {
	target := runtime.GOOS + "-" + runtime.GOARCH
	url := strings.TrimRight(base, "/") + "/download/cli/" + target
	binary, err := cliGet(ctx, url, maxCLIBinarySize)
	if err != nil {
		return err
	}
	sidecar, err := cliGet(ctx, url+".sha256", 1024)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(sidecar))
	sum := sha256.Sum256(binary)
	if len(fields) == 0 || !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
		return errors.New("downloaded binary does not match its .sha256")
	}
	pattern := ".dynapp-update-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	staged, err := os.CreateTemp(filepath.Dir(install.Path), pattern)
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if _, err := staged.Write(binary); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Chmod(staged.Name(), 0o755); err != nil {
		return err
	}
	if got := installedCLIVersion(ctx, install, staged.Name()); got != version {
		return fmt.Errorf("downloaded binary reports version %q, want %s", got, version)
	}
	return installCLIBinary(staged.Name(), install.Path)
}
