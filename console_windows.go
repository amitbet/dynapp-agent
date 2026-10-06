//go:build windows

package main

import (
	"log"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows releases use the GUI subsystem so Explorer never allocates a
// console for document opens, shortcuts, or URL handlers. CLI invocations
// attach to an existing parent console without creating a new one.
func prepareConsole(args []string) {
	if !needsParentConsole(args) {
		detachConsole() // Also keeps ordinary `go build` development binaries quiet.
		return
	}
	result, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole").Call(^uintptr(0))
	if result == 0 {
		return // Already attached, or the parent has no console. Preserve pipes.
	}
	// os initializes these before AttachConsole. Rebind missing handles, but
	// retain redirected pipes and files used by installers and CLI callers.
	for _, stream := range []struct {
		file **os.File
		kind uint32
	}{
		{&os.Stdin, windows.STD_INPUT_HANDLE},
		{&os.Stdout, windows.STD_OUTPUT_HANDLE},
		{&os.Stderr, windows.STD_ERROR_HANDLE},
	} {
		if _, err := windows.GetFileType(windows.Handle((*stream.file).Fd())); err == nil {
			continue
		}
		if handle, err := windows.GetStdHandle(stream.kind); err == nil && handle != 0 && handle != windows.InvalidHandle {
			name := "console"
			if *stream.file != nil {
				name = (*stream.file).Name()
			}
			*stream.file = os.NewFile(uintptr(handle), name)
		}
	}
	log.SetOutput(os.Stderr)
}

func needsParentConsole(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "-help" || arg == "--version" || arg == "-version" {
			return true
		}
	}
	for _, arg := range args {
		switch arg {
		case "app", "launch-app", "open-url", "presentation-helper", "file-promise-helper":
			return false
		}
		flag := strings.TrimPrefix(arg, "-")
		flag = strings.TrimPrefix(flag, "-")
		if flag == "background" || flag == "background=true" || flag == "update-helper" || flag == "update-helper=true" {
			return false
		}
	}
	return true
}

// detachConsole drops the console window a per-user agent started at sign-in
// would otherwise keep open.
func detachConsole() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
}
