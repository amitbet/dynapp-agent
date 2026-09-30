//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CurrentUser returns the active console user's environment when the agent
// runs as the LocalSystem service, and the agent's own otherwise.
func CurrentUser() (*UserEnvironment, error) {
	if !runningAsLocalSystem() {
		return &UserEnvironment{}, nil
	}
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
	env, err := primary.Environ(false)
	if err != nil {
		primary.Close()
		return nil, fmt.Errorf("create active user environment: %w", err)
	}
	user := &UserEnvironment{env: env, Impersonated: true}
	user.token = primary
	user.release = func() { primary.Close() }
	return user, nil
}

func runningAsLocalSystem() bool {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && tokenUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// LookPath resolves name against the user's PATH and PATHEXT rather than the
// service's machine-only PATH.
func (u *UserEnvironment) LookPath(name string) (string, error) {
	if u.env == nil {
		return exec.LookPath(name)
	}
	extensions := []string{""}
	if filepath.Ext(name) == "" {
		pathext := u.Getenv("PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		extensions = nil
		for _, ext := range strings.Split(pathext, ";") {
			if ext = strings.TrimSpace(ext); ext != "" {
				extensions = append(extensions, strings.ToLower(ext))
			}
		}
	}
	directories := filepath.SplitList(u.Getenv("PATH"))
	if strings.ContainsAny(name, `\/:`) {
		directories = []string{""}
	}
	for _, directory := range directories {
		if directory = strings.Trim(directory, `"`); directory == "" && !strings.ContainsAny(name, `\/:`) {
			continue
		}
		for _, ext := range extensions {
			candidate := filepath.Join(directory, name+ext)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", &exec.Error{Name: name, Err: errors.New("executable file not found in the user's PATH")}
}

// Command builds a command that runs with the user's token and environment.
// A nil ctx means no cancellation.
func (u *UserEnvironment) Command(ctx context.Context, path string, args ...string) *exec.Cmd {
	var command *exec.Cmd
	if ctx == nil {
		command = exec.Command(path, args...)
	} else {
		command = exec.CommandContext(ctx, path, args...)
	}
	if u.env != nil {
		command.Env = u.Environ()
		command.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(u.token), HideWindow: true, CreationFlags: createNoWindow}
	}
	return command
}

type userToken = windows.Token
