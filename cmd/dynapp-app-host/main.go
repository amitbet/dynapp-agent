//go:build linux && cgo && webkitgtk

// Command dynapp-app-host shows one installed DynApp in its own GTK window
// on Linux. The agent writes .desktop entries that start it; it is a separate
// binary so the agent itself needs no GUI libraries.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/amitbet/dynapp-agent/shellagent/linuxhost"
)

func init() { runtime.LockOSThread() }

func main() {
	options, err := linuxhost.ParseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if options.Version {
		fmt.Println(linuxhost.Version)
		return
	}
	if options.Check {
		if err := linuxhost.Check(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	os.Exit(linuxhost.Run(options))
}
