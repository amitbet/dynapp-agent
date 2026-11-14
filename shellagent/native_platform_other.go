//go:build !darwin && !windows && !android

package shellagent

import (
	"errors"
	"net"
	"os"
	"path/filepath"
)

var errNativeUnsupported = errors.New("native apps are not supported on this platform")

func nativePlatformName() string { return "unsupported" }

func nativeRequiresOptIn() bool { return false }

func nativeSupported() (bool, string) {
	return false, "native apps are available on macOS and Windows"
}

func nativeEndpoint(string) string { return "" }

func nativeListen(string) (net.Listener, error) { return nil, errNativeUnsupported }

func nativeVerifyPeer(net.Conn, NativeApp) error { return errNativeUnsupported }

func nativeInstall(nativeInstallSpec) (nativeInstallResult, error) {
	return nativeInstallResult{}, errNativeUnsupported
}

func nativeUninstall(NativeApp) error { return errNativeUnsupported }

func nativeLaunch(NativeApp, string) error { return errNativeUnsupported }

func xdgDataHome() (string, error) {
	if data := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(data) {
		return data, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// codingAgentsDir holds installed-apps.md and the local skill copy.
func codingAgentsDir() (string, error) {
	data, err := xdgDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "dynapp", "agents"), nil
}

// legacyAppNotesDir is where agents 0.1.59-0.1.61 wrote per-app notes.
func legacyAppNotesDir() (string, error) {
	data, err := xdgDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "dynapp", "apps"), nil
}

// writeNativeAppNote: no native installs exist here.
func writeNativeAppNote(NativeApp, appNote) {}

// codingAgentHomes is the user's home folder and CODEX_HOME, if set.
func codingAgentHomes() (string, string, error) {
	home, err := os.UserHomeDir()
	return home, os.Getenv("CODEX_HOME"), err
}
