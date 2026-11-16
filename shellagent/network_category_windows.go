//go:build windows

package shellagent

import (
	"context"
	"os/exec"
	"syscall"
)

func readPlatformNetworkProfiles(ctx context.Context) ([]networkProfile, error) {
	script := `Get-NetConnectionProfile -ErrorAction Stop|Select-Object Name,InterfaceAlias,@{n='NetworkCategory';e={[string]$_.NetworkCategory}}|ConvertTo-Json -Compress`
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return parseWindowsNetworkProfiles(output)
}
