//go:build windows

package shellagent

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceConfigFile renames the new config over the old one. On Windows that
// fails with "Access is denied" while another process has config.json open
// (an antivirus scan, a file viewer, another agent) or when it is read-only,
// so it clears the read-only flag and retries for a few seconds.
func replaceConfigFile(from, to string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if attempt == 0 {
			if path, convErr := windows.UTF16PtrFromString(to); convErr == nil {
				if attributes, attrErr := windows.GetFileAttributes(path); attrErr == nil && attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
					_ = windows.SetFileAttributes(path, attributes&^windows.FILE_ATTRIBUTE_READONLY)
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("%w (is another DynApp agent running, or is antivirus blocking dynapp-shell-agent.exe from changing this file?)", err)
}
