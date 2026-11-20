//go:build !darwin && !windows && !linux

package shellagent

import (
	"errors"
	"net"
)

var errNativeUnsupported = errors.New("native apps are not supported on this platform")

func nativePlatformName() string { return "unsupported" }

func nativeSupported() (bool, string) {
	return false, "native apps are available on macOS, Windows, and Linux"
}

func nativeEndpoint(string) string { return "" }

func nativeListen(string) (net.Listener, error) { return nil, errNativeUnsupported }

func nativeVerifyPeer(net.Conn, NativeApp) error { return errNativeUnsupported }

func nativeInstall(nativeInstallSpec) (nativeInstallResult, error) {
	return nativeInstallResult{}, errNativeUnsupported
}

func nativeUninstall(NativeApp) error { return errNativeUnsupported }

func nativeLaunch(NativeApp, string) error { return errNativeUnsupported }

// writeNativeAppNote: no native installs exist here.
func writeNativeAppNote(NativeApp, appNote) {}
