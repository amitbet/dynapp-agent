//go:build !windows

package main

import "os/exec"

func blockedHelper() *exec.Cmd { return exec.Command("/bin/sh", "-c", "exit 3") }
