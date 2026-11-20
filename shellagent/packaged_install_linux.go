//go:build linux && !android

package shellagent

import (
	"os"
	"path/filepath"
	"strings"
)

// dpkgInfoDir holds dpkg's per-package file lists; tests point it elsewhere.
var dpkgInfoDir = "/var/lib/dpkg/info"

// packagedInstallForExecutable reports whether dpkg's dynapp-shell-agent
// package owns the running executable.
func packagedInstallForExecutable() (packagedInstall, bool) {
	executable, err := os.Executable()
	if err != nil {
		return packagedInstall{}, false
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	lists, _ := filepath.Glob(filepath.Join(dpkgInfoDir, "dynapp-shell-agent*.list"))
	for _, list := range lists {
		name := strings.TrimSuffix(filepath.Base(list), ".list")
		if name != "dynapp-shell-agent" && !strings.HasPrefix(name, "dynapp-shell-agent:") {
			continue
		}
		data, err := os.ReadFile(list)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == executable {
				return packagedInstall{Manager: "apt", Executable: executable}, true
			}
		}
	}
	return packagedInstall{}, false
}
