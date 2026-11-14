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

// appNotesDir holds one coding-agent note per installed DynApp. Linux has no
// native host, so these notes describe browser installs.
func appNotesDir() (string, error) {
	if data := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(data) {
		return filepath.Join(data, "dynapp", "apps"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "dynapp", "apps"), nil
}

// writeNativeAppNotes: no native installs exist here.
func writeNativeAppNotes(NativeApp, appNote) {}

// codingAgentHomes is the user's home folder and CODEX_HOME, if set.
func codingAgentHomes() (string, string, error) {
	home, err := os.UserHomeDir()
	return home, os.Getenv("CODEX_HOME"), err
}
