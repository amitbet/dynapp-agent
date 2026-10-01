//go:build !darwin && !windows

package shellagent

import (
	"errors"
	"net"
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
