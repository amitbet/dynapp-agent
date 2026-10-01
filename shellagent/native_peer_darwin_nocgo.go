//go:build darwin && !cgo

package shellagent

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// processExecutablePath falls back to ps for CGo-disabled development builds.
func processExecutablePath(pid int) (string, error) {
	output, err := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	path := strings.TrimSpace(string(output))
	if err != nil || path == "" {
		return "", errors.New("could not read the peer process path")
	}
	return path, nil
}
