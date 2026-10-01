//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

func runningAsLocalSystem() bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// urlHandlerCommand is the registry open command for dynapp://.
func urlHandlerCommand(executable string) string {
	return `"` + executable + `" open-url "%1"`
}

// registerURLHandler writes dynapp:// under HKCU for a per-user agent, or
// HKLM for the LocalSystem service so every signed-in user gets it.
func registerURLHandler(_ string, executable string) error {
	root := registry.CURRENT_USER
	if runningAsLocalSystem() {
		root = registry.LOCAL_MACHINE
	}
	values := map[string]map[string]string{
		`Software\Classes\` + urlScheme:                         {"": "URL:DynApp agent", "URL Protocol": ""},
		`Software\Classes\` + urlScheme + `\DefaultIcon`:        {"": `"` + executable + `",0`},
		`Software\Classes\` + urlScheme + `\shell\open\command`: {"": urlHandlerCommand(executable)},
	}
	for path, entries := range values {
		key, _, err := registry.CreateKey(root, path, registry.SET_VALUE|registry.QUERY_VALUE)
		if err != nil {
			return err
		}
		for name, value := range entries {
			if current, _, err := key.GetStringValue(name); err == nil && current == value {
				continue
			}
			if err := key.SetStringValue(name, value); err != nil {
				key.Close()
				return err
			}
		}
		key.Close()
	}
	return nil
}

// startInstalledAgent starts the machine service when it exists (signed-in
// users get SERVICE_START from ensureServiceAccess), else a per-user agent.
func startInstalledAgent(executable string) error {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err == nil {
		defer windows.CloseServiceHandle(manager)
		name, _ := windows.UTF16PtrFromString(agentServiceName)
		service, openErr := windows.OpenService(manager, name, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS)
		if openErr == nil {
			defer windows.CloseServiceHandle(service)
			if startErr := windows.StartService(service, 0, nil); startErr != nil && !errors.Is(startErr, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return fmt.Errorf("start the DynApp agent service: %w", startErr)
			}
			return nil
		}
		if !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("open the DynApp agent service: %w", openErr)
		}
	}
	command := exec.Command(executable, "--background")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// serviceRecoveryReady is set once Windows is known to restart the service
// after it exits with an error; the updater relies on that to restart into a
// new binary without a helper process.
var serviceRecoveryReady bool

// ensureServiceRecovery runs in the LocalSystem service: restart on failure
// (also for a clean exit with a failure code), and let signed-in users start
// the service so dynapp://start works for them.
func ensureServiceRecovery() error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(agentServiceName)
	if err != nil {
		return err
	}
	defer service.Close()
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
	if err := service.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		return err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	current, err := service.RecoveryActions()
	if err != nil || len(current) == 0 || current[0].Type != mgr.ServiceRestart {
		return errors.New("Windows did not keep the service recovery actions")
	}
	serviceRecoveryReady = true
	return ensureServiceAccess()
}

// ensureServiceAccess grants interactive users (IU) SERVICE_START ("RP").
func ensureServiceAccess() error {
	sc := os.Getenv("SystemRoot") + `\System32\sc.exe`
	output, err := exec.Command(sc, "sdshow", agentServiceName).Output()
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(output))
	updated := withInteractiveStartRight(current)
	if updated == current {
		return nil
	}
	if output, err := exec.Command(sc, "sdset", agentServiceName, updated).CombinedOutput(); err != nil {
		return fmt.Errorf("allow signed-in users to start the agent: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// prepareServiceRecovery configures the LocalSystem service. A per-user agent
// has no service to configure.
func prepareServiceRecovery() error {
	if !runningAsLocalSystem() {
		return nil
	}
	return ensureServiceRecovery()
}
