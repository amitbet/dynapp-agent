package shellagent

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type AssociationOptions struct {
	AppID, Name, URL, Executable string
	Extensions                   []string
}

func (options AssociationOptions) Validate() error {
	if safeName(options.AppID) != options.AppID || options.AppID == "" {
		return errors.New("app id is invalid")
	}
	if strings.TrimSpace(options.Name) == "" {
		return errors.New("app name is required")
	}
	if !strings.HasPrefix(options.URL, "https://") && !strings.HasPrefix(options.URL, "http://127.0.0.1:") {
		return errors.New("app URL must use HTTPS")
	}
	if !filepath.IsAbs(options.Executable) {
		return errors.New("agent executable must be absolute")
	}
	if len(options.Extensions) == 0 {
		return errors.New("at least one file extension is required")
	}
	for _, extension := range options.Extensions {
		if safeName(extension) != extension || extension == "" {
			return errors.New("file extension is invalid")
		}
	}
	return nil
}
func InstallAssociations(options AssociationOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	existing, err := platformAssociationExtensions(options.AppID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if err := removePlatformAssociations(options.AppID); err != nil {
			return err
		}
	}
	return installPlatformAssociations(options)
}
func RemoveAssociations(appID string) error { return removePlatformAssociations(appID) }
func AssociationState(appID string) (map[string]any, error) {
	extensions, err := platformAssociationExtensions(appID)
	if err != nil {
		return nil, err
	}
	installed := len(extensions) > 0
	applied := extensions
	if runtime.GOOS == "darwin" && installed {
		applied, err = macDefaultAssociations("get", "com.dynapp.pwa."+safeName(appID), extensions)
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"supported": true, "installed": installed, "applied": installed && len(applied) == len(extensions), "partiallyApplied": len(applied) > 0 && len(applied) < len(extensions), "appliedExtensions": applied, "extensions": extensions, "platform": runtime.GOOS}, nil
}
func uniqueExtensions(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimPrefix(strings.TrimSpace(value), ".")
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func installDarwinAssociations(options AssociationOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bundle := filepath.Join(home, "Applications", "DynApp", safeName(options.Name)+".app")
	bundleName := options.Name + " file handler"
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		return err
	}
	// Finder sends open-document Apple events, not command-line arguments.
	// A compiled droplet receives those events and forwards every path to the agent.
	script := darwinAssociationLauncher(options)
	if output, err := exec.Command("/usr/bin/osacompile", "-o", bundle, "-e", script).CombinedOutput(); err != nil {
		return fmt.Errorf("build macOS file launcher: %w: %s", err, output)
	}
	extensions := ""
	utis := "<key>UTExportedTypeDeclarations</key><array>"
	for _, extension := range options.Extensions {
		extensions += "<string>" + html.EscapeString(extension) + "</string>"
		uti := "com.dynapp.pwa." + options.AppID + "." + extension
		utis += "<dict><key>UTTypeIdentifier</key><string>" + html.EscapeString(uti) + "</string><key>UTTypeConformsTo</key><array><string>public.data</string></array><key>UTTypeTagSpecification</key><dict><key>public.filename-extension</key><string>" + html.EscapeString(extension) + "</string></dict></dict>"
	}
	utis += "</array>"
	// Keep the extension list as the document claim. Launch Services can then
	// associate each extension with its existing preferred UTI (for example,
	// Markdown's public UTI) instead of treating the custom UTI as a replacement.
	plist := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleName</key><string>` + html.EscapeString(bundleName) + `</string><key>CFBundleDisplayName</key><string>` + html.EscapeString(bundleName) + `</string><key>CFBundleIdentifier</key><string>com.dynapp.pwa.` + options.AppID + `</string><key>CFBundleVersion</key><string>1</string><key>CFBundlePackageType</key><string>APPL</string>` + utis + `<key>CFBundleExecutable</key><string>droplet</string><key>CFBundleDocumentTypes</key><array><dict><key>CFBundleTypeName</key><string>DynApp Document</string><key>CFBundleTypeRole</key><string>Viewer</string><key>LSHandlerRank</key><string>Owner</string><key>CFBundleTypeExtensions</key><array>` + extensions + `</array></dict></array></dict></plist>`
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		return err
	}
	if output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", bundle).CombinedOutput(); err != nil {
		return fmt.Errorf("sign macOS file launcher: %w: %s", err, output)
	}
	lsregister := "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
	if output, err := exec.Command(lsregister, "-f", bundle).CombinedOutput(); err != nil {
		return fmt.Errorf("register macOS app: %w: %s", err, output)
	}
	_, err = macDefaultAssociations("apply", "com.dynapp.pwa."+options.AppID, options.Extensions, bundle)
	return err
}
func removeDarwinAssociations(appID string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, "Applications", "DynApp")
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		plist := filepath.Join(root, entry.Name(), "Contents", "Info.plist")
		data, _ := os.ReadFile(plist)
		if strings.Contains(string(data), "com.dynapp.pwa."+appID+"</string>") {
			bundle := filepath.Join(root, entry.Name())
			lsregister := "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
			if output, err := exec.Command(lsregister, "-u", bundle).CombinedOutput(); err != nil {
				return fmt.Errorf("unregister macOS app: %w: %s", err, output)
			}
			return os.RemoveAll(bundle)
		}
	}
	return nil
}

func darwinAssociationLauncher(options AssociationOptions) string {
	command := shellQuote(options.Executable) + " --app-id " + shellQuote(options.AppID) + " --app-url " + shellQuote(options.URL)
	if options.Name != "" {
		command += " --app-name " + shellQuote(options.Name)
	}
	command += " launch-app"
	// Escape the AppleScript string separately from the shell arguments it holds.
	literal := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(command)
	return `on launchFiles(openedFiles)
 set commandText to "` + literal + `"
 repeat with openedFile in openedFiles
  set commandText to commandText & " " & quoted form of (POSIX path of openedFile)
 end repeat
 do shell script commandText
end launchFiles
on run
 my launchFiles({})
end run
on open openedFiles
 my launchFiles(openedFiles)
end open
`
}
