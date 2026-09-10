package fs

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
)

func detached(command string, args []string, cwd string) error {
	path, err := exec.LookPath(command)
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Start()
}

func openDesktopPath(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return detached("open", []string{path}, "")
	case "windows":
		return detached("rundll32.exe", []string{"url.dll,FileProtocolHandler", path}, "")
	default:
		return detached("xdg-open", []string{path}, "")
	}
}

func trashPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("osascript", "-e", `on run argv`, "-e", `tell application \"Finder\" to delete POSIX file (item 1 of argv)`, "-e", `end run`, path).Run()
	case "windows":
		script := `Add-Type -AssemblyName Microsoft.VisualBasic; if ((Get-Item -LiteralPath $args[0]).PSIsContainer) {[Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory($args[0],'OnlyErrorDialogs','SendToRecycleBin')} else {[Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile($args[0],'OnlyErrorDialogs','SendToRecycleBin')}`
		return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script, path).Run()
	default:
		for _, candidate := range [][]string{{"gio", "trash", path}, {"trash-put", path}} {
			if _, err := exec.LookPath(candidate[0]); err == nil {
				return exec.Command(candidate[0], candidate[1:]...).Run()
			}
		}
		return errors.New("no freedesktop trash implementation is installed (gio or trash-put)")
	}
}

func desktopOpenWithOptions() []map[string]any {
	rows := []map[string]any{}
	editors := []struct{ id, label, icon, command, mac string }{{"cursor", "Cursor", "cursor", "cursor", "Cursor"}, {"vscode", "VS Code", "vscode", "code", "Visual Studio Code"}}
	for _, editor := range editors {
		available := false
		if runtime.GOOS == "darwin" {
			available = exec.Command("open", "-Ra", editor.mac).Run() == nil
		} else {
			_, err := exec.LookPath(editor.command)
			available = err == nil
		}
		if available {
			rows = append(rows, map[string]any{"id": editor.id, "label": editor.label, "icon": editor.icon})
		}
	}
	label := "Files"
	if runtime.GOOS == "darwin" {
		label = "Finder"
	} else if runtime.GOOS == "windows" {
		label = "Explorer"
	}
	return append(rows, map[string]any{"id": "file-manager", "label": label, "icon": "folder"})
}

func openDesktopPathWith(path, opener string) error {
	if opener == "file-manager" || opener == "" {
		return openDesktopPath(path)
	}
	command, app := "", ""
	switch opener {
	case "cursor":
		command, app = "cursor", "Cursor"
	case "vscode":
		command, app = "code", "Visual Studio Code"
	default:
		return fmt.Errorf("unknown open-with target: %s", opener)
	}
	if runtime.GOOS == "darwin" {
		return detached("open", []string{"-a", app, path}, "")
	}
	return detached(command, []string{path}, "")
}

func openTerminal(command, cwd string) error {
	if cwd == "" {
		cwd = agentutil.HomeDir()
	}
	switch runtime.GOOS {
	case "darwin":
		dir, err := os.MkdirTemp("", "dynapp-cmd-")
		if err != nil {
			return err
		}
		if err = os.Chmod(dir, 0o700); err != nil {
			os.RemoveAll(dir)
			return err
		}
		path := filepath.Join(dir, "dynapp-cmd.command")
		quoted := "'" + strings.ReplaceAll(cwd, "'", "'\\''") + "'"
		script := fmt.Sprintf("#!/bin/bash\ncd %s\n%s\nexec \"${SHELL:-/bin/bash}\" -l\n", quoted, command)
		if err = os.WriteFile(path, []byte(script), 0o700); err != nil {
			os.RemoveAll(dir)
			return err
		}
		if err = detached("open", []string{path}, ""); err != nil {
			os.RemoveAll(dir)
			return err
		}
		go func() {
			time.Sleep(15 * time.Second)
			_ = os.RemoveAll(dir)
		}()
		return nil
	case "windows":
		return detached("cmd.exe", []string{"/c", "start", "cmd.exe", "/k", command}, cwd)
	default:
		line := command + "; exec bash"
		candidates := [][]string{{"x-terminal-emulator", "-e", "bash", "-lc", line}, {"gnome-terminal", "--", "bash", "-lc", line}, {"konsole", "-e", "bash", "-lc", line}, {"xterm", "-e", "bash", "-lc", line}}
		for _, candidate := range candidates {
			if _, err := exec.LookPath(candidate[0]); err == nil {
				return detached(candidate[0], candidate[1:], cwd)
			}
		}
		return errors.New("no supported terminal emulator is installed")
	}
}

func createZip(sources []string, target string, options map[string]any) error {
	if len(sources) == 0 {
		return errors.New("archive has no source paths")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temporary := target + ".dynapp-tmp"
	file, err := os.Create(temporary)
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(temporary)
		}
	}()
	archive := zip.NewWriter(file)
	includeBase := options["includeBaseDir"] == true || options["includeRoot"] == true
	for _, source := range sources {
		absolute, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return err
		}
		base := filepath.Dir(absolute)
		if !includeBase && info.IsDir() {
			base = absolute
		}
		err = filepath.Walk(absolute, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == target || path == temporary {
				return nil
			}
			name, err := filepath.Rel(base, path)
			if err != nil || name == "." {
				return err
			}
			name = filepath.ToSlash(name)
			if info.IsDir() {
				name += "/"
			}
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name, header.Method = name, zip.Deflate
			writer, err := archive.CreateHeader(header)
			if err != nil || info.IsDir() {
				return err
			}
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, input)
			closeErr := input.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
		if err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	failed = false
	return nil
}

func watchSnapshot(root string) (map[string]any, error) {
	rows := []string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rows = append(rows, fmt.Sprintf("%s\x00%d\x00%d\x00%t", path, info.Size(), info.ModTime().UnixNano(), info.IsDir()))
		if len(rows) > 100000 {
			return errors.New("watched directory is too large")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rows)
	return map[string]any{"fingerprint": strings.Join(rows, "\n"), "checkedAt": time.Now().UnixMilli()}, nil
}
