//go:build !darwin

package shellagent

import "errors"

// Windows and Android hosts run from the agent binary itself, so an agent
// update already updates every installed app.

func nativeHostOutdated(NativeApp) bool { return false }

func nativeAppRunning(NativeApp) bool { return false }

func nativeUpgradeHost(NativeApp) (nativeInstallResult, error) {
	return nativeInstallResult{}, errors.New("this platform has no per-app host to upgrade")
}
