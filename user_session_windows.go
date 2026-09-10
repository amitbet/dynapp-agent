//go:build windows

package shellagent

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wtsCurrentServerHandle   = 0
	wtsActive                = 0
	securityImpersonation    = 2
	tokenPrimary             = 1
	createUnicodeEnvironment = 0x00000400
	createNoWindow           = 0x08000000
)

var (
	wtsapi32                  = windows.NewLazySystemDLL("wtsapi32.dll")
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procWTSEnumerateSessionsW = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQueryUserToken     = wtsapi32.NewProc("WTSQueryUserToken")
	procWTSFreeMemory         = wtsapi32.NewProc("WTSFreeMemory")
	procDuplicateTokenEx      = advapi32.NewProc("DuplicateTokenEx")
	procCreateProcessAsUserW  = advapi32.NewProc("CreateProcessAsUserW")
)

type wtsSessionInfo struct {
	SessionID uint32
	Station   *uint16
	State     uint32
}

// startUserSessionProcess starts a GUI helper in the active interactive
// desktop, even when the caller is a Windows service in Session 0. The helper
// inherits the signed-in user's token and environment, never the service's.
func startUserSessionProcess(executable string, args []string, workingDirectory string) error {
	_, err := launchUserSessionProcess(executable, args, workingDirectory, nil)
	return err
}

func launchUserSessionProcess(executable string, args []string, workingDirectory string, extraEnvironment []string) (func(), error) {
	sessionID, err := activeWTSSessionID()
	if err != nil {
		return nil, err
	}
	var impersonation windows.Handle
	if result, _, callErr := procWTSQueryUserToken.Call(uintptr(sessionID), uintptr(unsafe.Pointer(&impersonation))); result == 0 {
		return nil, fmt.Errorf("query active user token: %w", callErr)
	}
	defer windows.CloseHandle(impersonation)

	var primary windows.Token
	if result, _, callErr := procDuplicateTokenEx.Call(
		uintptr(impersonation), uintptr(windows.MAXIMUM_ALLOWED), 0,
		securityImpersonation, tokenPrimary, uintptr(unsafe.Pointer(&primary)),
	); result == 0 {
		return nil, fmt.Errorf("duplicate active user token: %w", callErr)
	}
	defer primary.Close()

	environment, err := primary.Environ(false)
	if err != nil {
		return nil, fmt.Errorf("create active user environment: %w", err)
	}
	// Keep the one-use channel credential out of the process command line.
	environment = append(environment, extraEnvironment...)
	var environmentBlock []uint16
	for _, entry := range environment {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		environmentBlock = append(environmentBlock, encoded...)
	}
	environmentBlock = append(environmentBlock, 0)

	commandLine := windows.StringToUTF16(quoteWindowsCommandLine(append([]string{executable}, args...)))
	applicationName := windows.StringToUTF16Ptr(executable)
	var directory *uint16
	if workingDirectory != "" {
		directory = windows.StringToUTF16Ptr(workingDirectory)
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: windows.StringToUTF16Ptr("winsta0\\default")}
	var process windows.ProcessInformation
	if result, _, callErr := procCreateProcessAsUserW.Call(
		uintptr(primary), uintptr(unsafe.Pointer(applicationName)), uintptr(unsafe.Pointer(&commandLine[0])),
		0, 0, 0, createUnicodeEnvironment|createNoWindow, uintptr(unsafe.Pointer(&environmentBlock[0])), uintptr(unsafe.Pointer(directory)),
		uintptr(unsafe.Pointer(&startup)), uintptr(unsafe.Pointer(&process)),
	); result == 0 {
		return nil, fmt.Errorf("start helper in active user session: %w", callErr)
	}
	defer windows.CloseHandle(process.Thread)
	var processMu sync.Mutex
	go func() {
		windows.WaitForSingleObject(process.Process, windows.INFINITE)
		processMu.Lock()
		defer processMu.Unlock()
		windows.CloseHandle(process.Process)
		process.Process = 0
	}()
	return func() {
		processMu.Lock()
		defer processMu.Unlock()
		if process.Process != 0 {
			_ = windows.TerminateProcess(process.Process, 0)
		}
	}, nil
}

func activeWTSSessionID() (uint32, error) {
	var entries *wtsSessionInfo
	var count uint32
	if result, _, callErr := procWTSEnumerateSessionsW.Call(wtsCurrentServerHandle, 0, 1, uintptr(unsafe.Pointer(&entries)), uintptr(unsafe.Pointer(&count))); result == 0 {
		return 0, fmt.Errorf("enumerate Windows sessions: %w", callErr)
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(entries)))
	for index := uint32(0); index < count; index++ {
		entry := (*wtsSessionInfo)(unsafe.Add(unsafe.Pointer(entries), uintptr(index)*unsafe.Sizeof(wtsSessionInfo{})))
		if entry.State == wtsActive && entry.SessionID != 0 {
			return entry.SessionID, nil
		}
	}
	return 0, fmt.Errorf("no active Windows user session")
}

func quoteWindowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for index, argument := range args {
		if argument == "" {
			quoted[index] = `""`
			continue
		}
		if !strings.ContainsAny(argument, " \t\n\v\"") {
			quoted[index] = argument
			continue
		}
		var value strings.Builder
		value.WriteByte('"')
		backslashes := 0
		for _, character := range argument {
			if character == '\\' {
				backslashes++
				continue
			}
			if character == '"' {
				value.WriteString(strings.Repeat(`\`, backslashes*2+1))
				value.WriteByte('"')
				backslashes = 0
				continue
			}
			value.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			value.WriteRune(character)
		}
		value.WriteString(strings.Repeat(`\`, backslashes*2))
		value.WriteByte('"')
		quoted[index] = value.String()
	}
	return strings.Join(quoted, " ")
}
