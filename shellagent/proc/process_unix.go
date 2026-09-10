//go:build !windows

package proc

import "syscall"

func parseSignal(value string) syscall.Signal {
	switch value {
	case "SIGINT":
		return syscall.SIGINT
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGHUP":
		return syscall.SIGHUP
	default:
		return syscall.SIGTERM
	}
}
