//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

var execve = syscall.Exec

func detachUpdateHelper(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func wrapHelperCommand(helperPath string, args []string) *exec.Cmd {
	if runtime.GOOS == "linux" {
		if systemdRun, err := exec.LookPath("systemd-run"); err == nil {
			wrapped := append([]string{"--user", "--scope", "--collect", helperPath}, args...)
			return exec.Command(systemdRun, wrapped...)
		}
	}
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
	if p.serviceMode {
		return p.scheduleHelperUpdate(downloadedPath, executable, version)
	}
	return p.execInstalledUpdate(downloadedPath, executable, version)
}

func (p *program) execInstalledUpdate(downloadedPath, executable, version string) error {
	cleanupStaleUpdateArtifacts(filepath.Dir(downloadedPath), executable)
	preparedPath, err := prepareReplacement(downloadedPath, executable)
	if err != nil {
		return err
	}
	log.Printf("DynApp Shell agent: applying release %s", version)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownContext); err != nil {
		_ = os.Remove(preparedPath)
		return fmt.Errorf("stop agent for update: %w", err)
	}
	backup, err := replaceExecutable(preparedPath, executable)
	if err != nil {
		log.Printf("DynApp Shell agent: installing update %s failed: %v", version, err)
		osExit(1)
		return err
	}
	argv := append([]string{executable}, os.Args[1:]...)
	if err := execve(executable, argv, os.Environ()); err != nil {
		if restoreErr := os.Rename(backup, executable); restoreErr != nil {
			log.Printf("DynApp Shell agent: could not restore previous agent: %v", restoreErr)
		} else {
			_ = os.Remove(backup)
		}
		log.Printf("DynApp Shell agent: exec of update %s failed: %v", version, err)
		osExit(1)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
