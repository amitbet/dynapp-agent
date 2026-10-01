//go:build windows

package main

import "golang.org/x/sys/windows"

// detachConsole drops the console window a per-user agent started at sign-in
// would otherwise keep open.
func detachConsole() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
}
