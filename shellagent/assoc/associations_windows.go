//go:build windows

package assoc

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
	"golang.org/x/sys/windows/registry"
)

func associationRegistryRoot() registry.Key {
	// A kardianos Windows service normally runs as LocalSystem. HKCU would then
	// register associations for the invisible service profile, so use the
	// machine Classes hive; an interactive/per-user agent keeps using HKCU.
	if strings.EqualFold(os.Getenv("USERNAME"), "SYSTEM") {
		return registry.LOCAL_MACHINE
	}
	return registry.CURRENT_USER
}

func installPlatformAssociations(options AssociationOptions) error {
	return installWindowsAssociations(options)
}
func removePlatformAssociations(appID string) error { return removeWindowsAssociations(appID) }
func platformAssociationInstalled(appID string) (bool, error) {
	extensions, err := platformAssociationExtensions(appID)
	return len(extensions) > 0, err
}
func platformAssociationExtensions(appID string) ([]string, error) {
	key, err := registry.OpenKey(associationRegistryRoot(), `Software\Classes\DynApp.`+agentutil.SafeName(appID), registry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer key.Close()
	values, _, err := key.GetStringsValue("Extensions")
	if err != nil {
		return nil, nil
	}
	return uniqueExtensions(values), nil
}

func installWindowsAssociations(options AssociationOptions) error {
	registryRoot := associationRegistryRoot()
	root, _, err := registry.CreateKey(registryRoot, `Software\Classes\DynApp.`+options.AppID, registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		return err
	}
	defer root.Close()
	_ = root.SetStringValue("", options.Name+" Document")
	_ = root.SetStringsValue("Extensions", uniqueExtensions(options.Extensions))
	command, _, err := registry.CreateKey(root, `shell\open\command`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer command.Close()
	if err := command.SetStringValue("", fmt.Sprintf(`"%s" --app-id "%s" --app-url "%s" launch-app "%%1"`, options.Executable, options.AppID, options.URL)); err != nil {
		return err
	}
	for _, extension := range options.Extensions {
		key, _, err := registry.CreateKey(registryRoot, `Software\Classes\.`+extension, registry.SET_VALUE)
		if err != nil {
			return err
		}
		_ = key.SetStringValue("", "DynApp."+options.AppID)
		key.Close()
	}
	return nil
}
func removeWindowsAssociations(appID string) error {
	return removeWindowsAssociationsAt(associationRegistryRoot(), appID)
}

func removeWindowsAssociationsAt(registryRoot registry.Key, appID string) error {
	appID = agentutil.SafeName(appID)
	var extensions []string
	key, err := registry.OpenKey(registryRoot, `Software\Classes\DynApp.`+appID, registry.QUERY_VALUE)
	if err == nil {
		extensions, _, err = key.GetStringsValue("Extensions")
		key.Close()
	}
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	for _, extension := range extensions {
		path := `Software\Classes\.` + extension
		key, err := registry.OpenKey(registryRoot, path, registry.QUERY_VALUE)
		if err == nil {
			current, _, _ := key.GetStringValue("")
			key.Close()
			if current == "DynApp."+appID {
				_ = registry.DeleteKey(registryRoot, path)
			}
		}
	}
	base := `Software\Classes\DynApp.` + appID
	for _, child := range []string{`shell\open\command`, `shell\open`, `shell`} {
		_ = registry.DeleteKey(registryRoot, base+`\`+child)
	}
	err = registry.DeleteKey(registryRoot, base)
	// The shell removes the previous set even on first install. An absent
	// registration already satisfies removal, including repeated requests.
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
