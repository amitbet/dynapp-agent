//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

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

// scheduleSelfUpdate never launches a copy of the agent when it can avoid it:
// behavior-based antivirus (Bitdefender Advanced Threat Defense, for example)
// blocks an unsigned service that starts an unsigned copy of itself.
//
//   - Service with recovery actions set: replace the binary in place (Windows
//     allows renaming a running .exe) and exit with a failure code; the
//     Service Control Manager restarts the service from the new file.
//   - Per-user agent: replace in place and start the installed .exe itself.
//   - Service without recovery actions: the original helper route.
func (p *program) scheduleSelfUpdate(_ context.Context, downloadedPath, version string) error {
	executable, err := installedExecutable()
	if err != nil {
		return err
	}
	if downloadedPath == executable {
		return fmt.Errorf("downloaded update has the same path as the running executable")
	}
	if p.serviceMode && !serviceRecoveryReady {
		return p.scheduleHelperUpdate(downloadedPath, executable, version)
	}
	return p.replaceInPlaceAndRestart(downloadedPath, executable, version)
}

func (p *program) replaceInPlaceAndRestart(downloadedPath, executable, version string) error {
	cleanupStaleUpdateArtifacts(filepath.Dir(downloadedPath), executable)
	preparedPath, err := prepareReplacement(downloadedPath, executable)
	if err != nil {
		return err
	}
	log.Printf("DynApp Shell agent: applying release %s in place", version)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownContext); err != nil {
		_ = os.Remove(preparedPath)
		return fmt.Errorf("stop agent for update: %w", err)
	}
	backup, err := replaceExecutable(preparedPath, executable)
	if err != nil {
		// The listener is closed; exit so the service restarts the old binary.
		log.Printf("DynApp Shell agent: installing update %s failed: %v", version, err)
		osExit(1)
		return err
	}
	if p.serviceMode {
		log.Printf("DynApp Shell agent: exiting so Windows restarts the service on %s", version)
		osExit(1)
		return nil
	}
	if err := startDetached(executable, os.Args[1:]); err != nil {
		log.Printf("DynApp Shell agent: starting %s failed (%v); restoring the previous agent", version, err)
		if restoreErr := os.Rename(backup, executable); restoreErr == nil {
			_ = startDetached(executable, os.Args[1:])
		}
		osExit(1)
		return err
	}
	osExit(0)
	return nil
}

// startDetached starts the installed agent with no console and no ties to
// this process.
func startDetached(executable string, args []string) error {
	command := exec.Command(executable, args...)
	command.Dir = filepath.Dir(executable)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
