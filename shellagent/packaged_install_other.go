//go:build !linux || android

package shellagent

func packagedInstallForExecutable() (packagedInstall, bool) { return packagedInstall{}, false }
