//go:build !windows

package proc

import "os/exec"

func ConfigureBackground(*exec.Cmd) {}
