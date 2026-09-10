//go:build darwin

package shellagent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func installPlatformAssociations(options AssociationOptions) error {
	return installDarwinAssociations(options)
}
func removePlatformAssociations(appID string) error { return removeDarwinAssociations(appID) }
func platformAssociationInstalled(appID string) (bool, error) {
	extensions, err := platformAssociationExtensions(appID)
	return len(extensions) > 0, err
}
func platformAssociationExtensions(appID string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
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
