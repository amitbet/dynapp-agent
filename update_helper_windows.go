//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func detachUpdateHelper(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

func wrapHelperCommand(helperPath string, args []string) *exec.Cmd {
	return exec.Command(helperPath, args...)
}

func (p *program) scheduleSelfUpdate(_ context.Context, downloadedPath, version string) error {
	executable, err := installedExecutable()
	if err != nil {
		return err
	}
	if downloadedPath == executable {
		return fmt.Errorf("downloaded update has the same path as the running executable")
	}
	return p.scheduleHelperUpdate(downloadedPath, executable, version)
}
