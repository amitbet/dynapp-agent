//go:build !darwin && !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func registerURLHandler(string, string) error { return nil }

// startInstalledAgent starts the systemd user unit, or the binary itself.
func startInstalledAgent(executable string) error {
	if exec.Command("systemctl", "--user", "start", agentServiceName).Run() == nil {
		return nil
	}
	command := exec.Command(executable)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	if command.Process == nil {
		return errors.New("the agent process did not start")
	}
	return command.Process.Release()
}

func prepareServiceRecovery() error { return nil }
