//go:build !(linux && cgo && webkitgtk)

package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("unsupported")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "dynapp-app-host is the Linux app host; build it on Linux with CGO_ENABLED=1 -tags webkitgtk")
	os.Exit(1)
}
