//go:build !windows

package shellagent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func installPlatformAssociations(options AssociationOptions) error {
	if runtime.GOOS == "darwin" {
		return installDarwinAssociations(options)
	}
	return installLinuxAssociations(options)
}
func removePlatformAssociations(appID string) error {
	if runtime.GOOS == "darwin" {
		return removeDarwinAssociations(appID)
	}
	return removeLinuxAssociations(appID)
}
func platformAssociationInstalled(appID string) (bool, error) {
	extensions, err := platformAssociationExtensions(appID)
	return len(extensions) > 0, err
}
func platformAssociationExtensions(appID string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		root := filepath.Join(home, "Applications", "DynApp")
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			data, _ := os.ReadFile(filepath.Join(root, entry.Name(), "Contents", "Info.plist"))
			text := string(data)
			if strings.Contains(text, "com.dynapp.pwa."+safeName(appID)+"</string>") {
				section := text
				if start := strings.Index(section, "<key>CFBundleTypeExtensions</key><array>"); start >= 0 {
					section = section[start+len("<key>CFBundleTypeExtensions</key><array>"):]
					if end := strings.Index(section, "</array>"); end >= 0 {
						section = section[:end]
					}
				}
				matches := regexp.MustCompile(`<string>([A-Za-z0-9_.-]+)</string>`).FindAllStringSubmatch(section, -1)
				extensions := []string{}
				for _, match := range matches {
					extensions = append(extensions, match[1])
				}
				return uniqueExtensions(extensions), nil
			}
		}
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".local", "share", "mime", "packages", "dynapp-"+safeName(appID)+".xml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	matches := regexp.MustCompile(`<glob pattern="\*\.([A-Za-z0-9_.-]+)"`).FindAllStringSubmatch(string(data), -1)
	extensions := []string{}
	for _, match := range matches {
		extensions = append(extensions, match[1])
	}
	return uniqueExtensions(extensions), nil
}

func installLinuxAssociations(options AssociationOptions) error {
	if runtime.GOOS == "darwin" {
		return installDarwinAssociations(options)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	applications := filepath.Join(home, ".local", "share", "applications")
	packages := filepath.Join(home, ".local", "share", "mime", "packages")
	if err := os.MkdirAll(applications, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(packages, 0o755); err != nil {
		return err
	}
	mimeTypes := []string{}
	globs := []string{}
	xmlTypes := []string{}
	for _, extension := range options.Extensions {
		mime := "application/x-dynapp-" + options.AppID + "-" + extension
		mimeTypes = append(mimeTypes, mime)
		globs = append(globs, "*."+extension)
		xmlTypes = append(xmlTypes, `<mime-type type="`+mime+`"><comment>`+options.Name+` document</comment><glob pattern="*.`+extension+`"/></mime-type>`)
	}
	desktop := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=%s\nExec=%s --app-id %s --app-url %s launch-app %%F\nTerminal=false\nMimeType=%s;\n", options.Name, desktopExecQuote(options.Executable), desktopExecQuote(options.AppID), desktopExecQuote(options.URL), strings.Join(mimeTypes, ";"))
	desktopPath := filepath.Join(applications, "dynapp-"+options.AppID+".desktop")
	if err := os.WriteFile(desktopPath, []byte(desktop), 0o755); err != nil {
		return err
	}
	xml := `<?xml version="1.0"?><mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info">` + strings.Join(xmlTypes, "") + `</mime-info>`
	if err := os.WriteFile(filepath.Join(packages, "dynapp-"+options.AppID+".xml"), []byte(xml), 0o644); err != nil {
		return err
	}
	_ = exec.Command("update-mime-database", filepath.Join(home, ".local", "share", "mime")).Run()
	for _, mime := range mimeTypes {
		_ = exec.Command("xdg-mime", "default", filepath.Base(desktopPath), mime).Run()
	}
	_ = globs
	return nil
}
func desktopExecQuote(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`").Replace(value)
	return `"` + value + `"`
}
func removeLinuxAssociations(appID string) error {
	if runtime.GOOS == "darwin" {
		return removeDarwinAssociations(appID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(home, ".local", "share", "applications", "dynapp-"+safeName(appID)+".desktop"))
	_ = os.Remove(filepath.Join(home, ".local", "share", "mime", "packages", "dynapp-"+safeName(appID)+".xml"))
	_ = exec.Command("update-mime-database", filepath.Join(home, ".local", "share", "mime")).Run()
	return nil
}
