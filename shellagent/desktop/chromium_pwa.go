package desktop

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
)

type chromiumPWARepair struct {
	home string
}

func isChromiumPWADesktopName(name string) bool {
	if !strings.HasSuffix(name, ".desktop") {
		return false
	}
	return strings.HasPrefix(name, "chrome-") || strings.HasPrefix(name, "chromium-") || strings.HasPrefix(name, "google-chrome")
}

func (repair chromiumPWARepair) dirs() []string {
	home := repair.home
	if home == "" {
		home = agentutil.HomeDir()
	}
	desktop := os.Getenv("XDG_DESKTOP_DIR")
	if desktop == "" || !filepath.IsAbs(desktop) {
		desktop = filepath.Join(home, "Desktop")
	}
	return []string{desktop, filepath.Join(home, ".local", "share", "applications")}
}

func (repair chromiumPWARepair) file(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	updated, changed := patchChromiumPWADesktop(string(data))
	if !changed {
		return false, nil
	}
	mode := info.Mode()
	if mode&0o111 == 0 {
		mode |= 0o700
	}
	return true, os.WriteFile(path, []byte(updated), mode)
}

func (repair chromiumPWARepair) once() int {
	patched := 0
	for _, dir := range repair.dirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !isChromiumPWADesktopName(entry.Name()) {
				continue
			}
			changed, err := repair.file(filepath.Join(dir, entry.Name()))
			if err != nil || !changed {
				continue
			}
			patched++
		}
	}
	return patched
}

func patchChromiumPWADesktop(contents string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n")
	changed := false
	for i, line := range lines {
		if !strings.HasPrefix(line, "Exec=") {
			continue
		}
		exec := strings.TrimPrefix(line, "Exec=")
		if !strings.Contains(exec, "--app-id=") || !chromiumPWAExecBinary(exec) {
			continue
		}
		next := ensureChromiumNoSandbox(exec)
		if next == exec {
			continue
		}
		lines[i] = "Exec=" + next
		changed = true
	}
	if !changed {
		return contents, false
	}
	return strings.Join(lines, "\n"), true
}

func chromiumPWAExecBinary(exec string) bool {
	base := strings.ToLower(filepath.Base(strings.Trim(chromiumPWABinary(exec), `"`)))
	switch base {
	case "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "google-chrome-beta", "chrome", "wrapped-chromium":
		return true
	default:
		return strings.Contains(base, "chrom")
	}
}

func chromiumPWABinary(exec string) string {
	exec = strings.TrimSpace(exec)
	if strings.HasPrefix(exec, `"`) {
		if end := strings.Index(exec[1:], `"`); end >= 0 {
			return exec[:end+2]
		}
	}
	if field, _, ok := strings.Cut(exec, " "); ok {
		return field
	}
	return exec
}

func ensureChromiumNoSandbox(exec string) string {
	if hasChromiumFlag(exec, "--no-sandbox") {
		return exec
	}
	binary := chromiumPWABinary(exec)
	return binary + " --no-sandbox" + strings.TrimPrefix(exec, binary)
}

func hasChromiumFlag(exec, flag string) bool {
	for _, field := range strings.Fields(exec) {
		if field == flag || strings.HasPrefix(field, flag+"=") {
			return true
		}
	}
	return false
}
