//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The per-user install lives in the agent itself rather than in PowerShell:
// antivirus script scanning (AMSI) blocks a script that downloads further
// script, writes a Run key, and starts a hidden unsigned program. The
// installer script now only downloads, verifies, and runs `install-user`.

const (
	runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue    = "DynApp Agent"
	userEnvPath = `Environment`
)

func perUserInstallDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "DynApp")
}

func serviceInstalled() bool {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(manager)
	name, _ := windows.UTF16PtrFromString(agentServiceName)
	service, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	windows.CloseServiceHandle(service)
	return true
}

// runInstallUser installs the running executable as the per-user agent,
// replacing a machine service first (one UAC prompt).
func runInstallUser(nativeApps bool) error {
	source, err := installedExecutable()
	if err != nil {
		return err
	}
	if serviceInstalled() || registryKeyExists(registry.LOCAL_MACHINE, `Software\Classes\`+urlScheme) {
		fmt.Println("Found the machine-wide DynApp agent service; replacing it with a per-user agent.")
		fmt.Println("Approve the Windows prompt to remove it (this is the only step that needs Administrator).")
		if err := runElevated(source, []string{"remove-service"}); err != nil {
			return fmt.Errorf("remove the machine-wide service: %w", err)
		}
		for attempt := 0; attempt < 20 && serviceInstalled(); attempt++ {
			time.Sleep(500 * time.Millisecond)
		}
		if serviceInstalled() {
			return errors.New("the machine-wide service is still installed, so the per-user agent was not installed")
		}
	}
	dir := perUserInstallDir()
	target := filepath.Join(dir, "dynapp-shell-agent.exe")
	stopProcessesAt(target)
	if !strings.EqualFold(filepath.Clean(source), filepath.Clean(target)) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := copyExecutable(source, target); err != nil {
			return fmt.Errorf("install %s: %w", target, err)
		}
	}
	if err := setUserRegistry(target, dir, nativeApps); err != nil {
		return err
	}
	environment := os.Environ()
	if nativeApps {
		environment = append(environment, "DYNAPP_NATIVE_APPS=1")
	}
	command := exec.Command(target, "--background")
	command.Dir = dir
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start the agent: %w", err)
	}
	_ = command.Process.Release()
	if !waitForAgent(defaultAgentAddress, agentStartTimeout) {
		return errors.New("the agent was installed but did not start; check %APPDATA%\\DynApp\\shell-agent\\logs\\agent.log")
	}
	fmt.Printf("Installed %s for %s. It is running and starts automatically when you sign in.\n", target, os.Getenv("USERNAME"))
	if nativeApps {
		fmt.Println("Native desktop apps are on.")
	}
	return nil
}

func registryKeyExists(root registry.Key, path string) bool {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	key.Close()
	return true
}

func copyExecutable(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	staged := target + ".new"
	output, err := os.OpenFile(staged, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	// A previous agent may still hold the file open for a moment.
	if _, err := os.Stat(target); err == nil {
		_ = os.Rename(target, target+fmt.Sprintf(".old-%d", os.Getpid()))
	}
	return os.Rename(staged, target)
}

// setUserRegistry adds the sign-in start, the user PATH entry, and the
// native-apps setting, and tells Explorer the environment changed.
func setUserRegistry(target, dir string, nativeApps bool) error {
	run, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = run.SetStringValue(runValue, `"`+target+`" --background`)
	run.Close()
	if err != nil {
		return err
	}
	environment, _, err := registry.CreateKey(registry.CURRENT_USER, userEnvPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer environment.Close()
	path, _, _ := environment.GetStringValue("Path")
	if !pathListContains(path, dir) {
		if path != "" && !strings.HasSuffix(path, ";") {
			path += ";"
		}
		if err := environment.SetExpandStringValue("Path", path+dir); err != nil {
			return err
		}
	}
	if nativeApps {
		if err := environment.SetStringValue("DYNAPP_NATIVE_APPS", "1"); err != nil {
			return err
		}
	}
	broadcastEnvironmentChange()
	return nil
}

func pathListContains(list, dir string) bool {
	for _, entry := range strings.Split(list, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(entry), `\`), strings.TrimRight(dir, `\`)) {
			return true
		}
	}
	return false
}

func pathListWithout(list, dir string) string {
	kept := []string{}
	for _, entry := range strings.Split(list, ";") {
		if strings.TrimSpace(entry) != "" && !strings.EqualFold(strings.TrimRight(strings.TrimSpace(entry), `\`), strings.TrimRight(dir, `\`)) {
			kept = append(kept, entry)
		}
	}
	return strings.Join(kept, ";")
}

var procSendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

func broadcastEnvironmentChange() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	environment, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	_, _, _ = procSendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(environment)), smtoAbortIfHung, 2000, uintptr(unsafe.Pointer(&result)))
}

// stopProcessesAt ends every process running the given executable (the agent
// and native app windows), except this one.
func stopProcessesAt(executable string) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	self := uint32(os.Getpid())
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ProcessID == self || !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), filepath.Base(executable)) {
			continue
		}
		process, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, entry.ProcessID)
		if openErr != nil {
			continue
		}
		buffer := make([]uint16, windows.MAX_LONG_PATH)
		size := uint32(len(buffer))
		if windows.QueryFullProcessImageName(process, 0, &buffer[0], &size) == nil &&
			strings.EqualFold(filepath.Clean(windows.UTF16ToString(buffer[:size])), filepath.Clean(executable)) {
			_ = windows.TerminateProcess(process, 0)
			_, _ = windows.WaitForSingleObject(process, 5000)
		}
		windows.CloseHandle(process)
	}
}

type shellExecuteInfo struct {
	size        uint32
	mask        uint32
	hwnd        uintptr
	verb        *uint16
	file        *uint16
	parameters  *uint16
	directory   *uint16
	show        int32
	instApp     uintptr
	idList      uintptr
	class       *uint16
	keyClass    uintptr
	hotKey      uint32
	iconMonitor uintptr
	process     windows.Handle
}

var procShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// runElevated runs this executable as Administrator (one UAC prompt) and
// waits for it.
func runElevated(executable string, args []string) error {
	const seeMaskNoCloseProcess = 0x00000040
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(executable)
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = windows.EscapeArg(arg)
	}
	parameters, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, verb: verb, file: file, parameters: parameters, show: windows.SW_HIDE}
	info.size = uint32(unsafe.Sizeof(info))
	if ok, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return errors.New("the Administrator prompt was declined")
		}
		return callErr
	}
	if info.process == 0 {
		return nil
	}
	defer windows.CloseHandle(info.process)
	_, _ = windows.WaitForSingleObject(info.process, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err == nil && code != 0 {
		return fmt.Errorf("the Administrator step exited with code %d", code)
	}
	return nil
}

// runRemoveService removes the machine service and what it added. It runs
// elevated (from install-user, or by hand from an Administrator prompt).
func runRemoveService() error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("open the service manager (run as Administrator): %w", err)
	}
	defer manager.Disconnect()
	programDir := filepath.Join(programFilesDir(), "DynApp")
	stateDir := ""
	if service, err := manager.OpenService(agentServiceName); err == nil {
		if config, err := service.Config(); err == nil {
			stateDir = stateDirFromCommandLine(config.BinaryPathName)
		}
		if status, err := service.Control(svc.Stop); err == nil {
			for attempt := 0; attempt < 30 && status.State != svc.Stopped; attempt++ {
				time.Sleep(500 * time.Millisecond)
				status, _ = service.Query()
			}
		}
		if err := service.Delete(); err != nil {
			service.Close()
			return fmt.Errorf("delete the service: %w", err)
		}
		service.Close()
	}
	stopProcessesAt(filepath.Join(programDir, "dynapp-shell-agent.exe"))
	for _, rule := range []string{"DynApp Shell Agent (UDP)", "DynApp Shell Agent LAN"} {
		_ = exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "netsh.exe"), "advfirewall", "firewall", "delete", "rule", "name="+rule).Run()
	}
	if environment, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if path, kind, err := environment.GetStringValue("Path"); err == nil {
			if updated := pathListWithout(path, programDir); updated != path {
				if kind == registry.EXPAND_SZ {
					_ = environment.SetExpandStringValue("Path", updated)
				} else {
					_ = environment.SetStringValue("Path", updated)
				}
			}
		}
		environment.Close()
	}
	deleteRegistryTree(registry.LOCAL_MACHINE, `Software\Classes\`+urlScheme)
	_ = os.RemoveAll(programDir)
	broadcastEnvironmentChange()
	fmt.Println("Removed the machine-wide DynApp agent service, its firewall rules, PATH entry, and dynapp:// handler.")
	if stateDir != "" {
		fmt.Println("Its data is kept in " + stateDir)
	}
	return nil
}

func programFilesDir() string {
	if dir := os.Getenv("ProgramW6432"); dir != "" {
		return dir
	}
	return os.Getenv("ProgramFiles")
}

func deleteRegistryTree(root registry.Key, path string) {
	key, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		names, _ := key.ReadSubKeyNames(-1)
		key.Close()
		for _, name := range names {
			deleteRegistryTree(root, path+`\`+name)
		}
	}
	_ = registry.DeleteKey(root, path)
}
