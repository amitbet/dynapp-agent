//go:build windows

package shellagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows app integration (Start menu and desktop shortcuts, Open with
// registrations) is done in-process with COM and the registry API. It used to
// run a PowerShell script with -ExecutionPolicy Bypass -EncodedCommand, which
// endpoint protection treats as malware behaviour and blocks.

var (
	integrationOle32          = windows.NewLazySystemDLL("ole32.dll")
	integrationShell32        = windows.NewLazySystemDLL("shell32.dll")
	integrationAdvapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCoCreateInstance      = integrationOle32.NewProc("CoCreateInstance")
	procSHChangeNotify        = integrationShell32.NewProc("SHChangeNotify")
	procRegOpenCurrentUser    = integrationAdvapi32.NewProc("RegOpenCurrentUser")
	procRegDeleteTreeW        = integrationAdvapi32.NewProc("RegDeleteTreeW")
	procRegSetValueExW        = integrationAdvapi32.NewProc("RegSetValueExW")
	clsidShellLink            = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW             = windows.GUID{Data1: 0x000214f9, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile            = windows.GUID{Data1: 0x0000010b, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPropertyStore          = windows.GUID{Data1: 0x886d8eeb, Data2: 0x8cf2, Data3: 0x4446, Data4: [8]byte{0x8d, 0x02, 0xcd, 0xba, 0x1d, 0xbd, 0xcf, 0x99}}
	pkeyAppUserModelID        = propertyKey{FmtID: windows.GUID{Data1: 0x9f4c2855, Data2: 0x9f79, Data3: 0x4b39, Data4: [8]byte{0xa8, 0xd0, 0xe1, 0xd4, 0x2d, 0xe1, 0xd5, 0xf3}}, PID: 5}
	errIntegrationKeyNotFound = syscall.Errno(windows.ERROR_FILE_NOT_FOUND)
)

const (
	clsctxInprocServer  = 1
	coinitApartment     = 2
	vtLPWStr            = 31
	swShowMinNoActive   = 7
	shcneAssocChanged   = 0x08000000
	shellLinkSetDesc    = 7
	shellLinkSetWorkDir = 9
	shellLinkSetArgs    = 11
	shellLinkSetShowCmd = 15
	shellLinkSetIcon    = 17
	shellLinkSetPath    = 20
	propertyStoreSet    = 6
	propertyStoreCommit = 7
	persistFileSave     = 6
	unknownQuery        = 0
	unknownRelease      = 2
)

type propertyKey struct {
	FmtID windows.GUID
	PID   uint32
}

// propVariant is a PROPVARIANT holding a VT_LPWSTR.
type propVariant struct {
	VT       uint16
	reserved [3]uint16
	Value    uintptr
	padding  uintptr
}

func comCall(object uintptr, method int, args ...uintptr) error {
	vtable := *(*uintptr)(unsafe.Pointer(object))
	function := *(*uintptr)(unsafe.Pointer(vtable + uintptr(method)*unsafe.Sizeof(uintptr(0))))
	result, _, _ := syscall.SyscallN(function, append([]uintptr{object}, args...)...)
	if int32(result) < 0 {
		return fmt.Errorf("HRESULT 0x%08x", uint32(result))
	}
	return nil
}

func comQuery(object uintptr, iid *windows.GUID) (uintptr, error) {
	var result uintptr
	err := comCall(object, unknownQuery, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&result)))
	return result, err
}

func comString(value string) (*uint16, error) { return windows.UTF16PtrFromString(value) }

// withCOM runs fn with COM initialised on the current (locked) thread.
func withCOM(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, coinitApartment); err {
	case nil, syscall.Errno(1): // S_OK, S_FALSE
		defer windows.CoUninitialize()
	case syscall.Errno(windows.RPC_E_CHANGED_MODE):
	default:
		return fmt.Errorf("initialise COM: %w", err)
	}
	return fn()
}

// windowsShortcut is a .lnk whose System.AppUserModel.ID matches the running
// host, so pinning and taskbar grouping work per app.
type windowsShortcut struct {
	Path, Target, Args, Icon, AppID, Description string
}

func (s windowsShortcut) create() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	return withCOM(func() error {
		var link uintptr
		if result, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInprocServer,
			uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link))); int32(result) < 0 {
			return fmt.Errorf("create shell link: HRESULT 0x%08x", uint32(result))
		}
		defer comCall(link, unknownRelease)
		for _, step := range []struct {
			method int
			value  string
		}{
			{shellLinkSetPath, s.Target}, {shellLinkSetArgs, s.Args}, {shellLinkSetDesc, s.Description},
			{shellLinkSetWorkDir, filepath.Dir(s.Target)},
		} {
			text, err := comString(step.value)
			if err != nil {
				return err
			}
			if err := comCall(link, step.method, uintptr(unsafe.Pointer(text))); err != nil {
				return fmt.Errorf("shell link method %d: %w", step.method, err)
			}
		}
		icon, err := comString(s.Icon)
		if err != nil {
			return err
		}
		if err := comCall(link, shellLinkSetIcon, uintptr(unsafe.Pointer(icon)), 0); err != nil {
			return fmt.Errorf("shortcut icon: %w", err)
		}
		if err := comCall(link, shellLinkSetShowCmd, swShowMinNoActive); err != nil {
			return fmt.Errorf("shortcut show command: %w", err)
		}
		store, err := comQuery(link, &iidPropertyStore)
		if err != nil {
			return fmt.Errorf("shortcut property store: %w", err)
		}
		defer comCall(store, unknownRelease)
		appID, err := comString(s.AppID)
		if err != nil {
			return err
		}
		value := propVariant{VT: vtLPWStr, Value: uintptr(unsafe.Pointer(appID))}
		if err := comCall(store, propertyStoreSet, uintptr(unsafe.Pointer(&pkeyAppUserModelID)), uintptr(unsafe.Pointer(&value))); err != nil {
			return fmt.Errorf("shortcut AppUserModelID: %w", err)
		}
		runtime.KeepAlive(appID)
		if err := comCall(store, propertyStoreCommit); err != nil {
			return fmt.Errorf("commit shortcut properties: %w", err)
		}
		persist, err := comQuery(link, &iidPersistFile)
		if err != nil {
			return fmt.Errorf("shortcut persist file: %w", err)
		}
		defer comCall(persist, unknownRelease)
		path, err := comString(s.Path)
		if err != nil {
			return err
		}
		if err := comCall(persist, persistFileSave, uintptr(unsafe.Pointer(path)), 1); err != nil {
			return fmt.Errorf("save shortcut: %w", err)
		}
		return nil
	})
}

// nativeWindowsIntegration registers or removes one app's desktop shortcut
// and Open with entries under Root\Base (HKCU\Software for real installs).
type nativeWindowsIntegration struct {
	Root       registry.Key
	Base       string
	Desktop    string
	Uninstall  bool
	ID, Name   string
	Extensions []string
	Shortcut   windowsShortcut // Start menu shortcut; install only
}

// currentUserRegistry opens HKCU for the thread's identity, which is the
// impersonated user under desktop.UserEnvironment.AsUser.
func currentUserRegistry() (registry.Key, error) {
	var key syscall.Handle
	if status, _, _ := procRegOpenCurrentUser.Call(uintptr(registry.ALL_ACCESS), uintptr(unsafe.Pointer(&key))); status != 0 {
		return 0, fmt.Errorf("open the user's registry: %w", syscall.Errno(status))
	}
	return registry.Key(key), nil
}

func deleteRegistryTree(root registry.Key, path string) error {
	text, err := comString(path)
	if err != nil {
		return err
	}
	if status, _, _ := procRegDeleteTreeW.Call(uintptr(root), uintptr(unsafe.Pointer(text))); status != 0 && syscall.Errno(status) != errIntegrationKeyNotFound {
		return fmt.Errorf("delete %s: %w", path, syscall.Errno(status))
	}
	return nil
}

func setRegistryString(root registry.Key, path, name, value string) error {
	key, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer key.Close()
	return key.SetStringValue(name, value)
}

func deleteRegistryValue(root registry.Key, path, name string) {
	key, err := registry.OpenKey(root, path, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	_ = key.DeleteValue(name)
}

func (n nativeWindowsIntegration) appPath() string { return n.Base + `\DynApp\NativeApps\` + n.ID }

func (n nativeWindowsIntegration) removeAssociations() error {
	if key, err := registry.OpenKey(n.Root, n.appPath()+`\Capabilities\FileAssociations`, registry.QUERY_VALUE); err == nil {
		extensions, _ := key.ReadValueNames(0)
		key.Close()
		for _, extension := range extensions {
			deleteRegistryValue(n.Root, n.Base+`\Classes\`+extension+`\OpenWithProgids`, n.ID)
		}
	}
	if err := deleteRegistryTree(n.Root, n.Base+`\Classes\`+n.ID); err != nil {
		return err
	}
	if err := deleteRegistryTree(n.Root, n.appPath()+`\Capabilities`); err != nil {
		return err
	}
	deleteRegistryValue(n.Root, n.Base+`\RegisteredApplications`, n.ID)
	return nil
}

func (n nativeWindowsIntegration) previousDesktopShortcut() string {
	key, err := registry.OpenKey(n.Root, n.appPath(), registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("DesktopShortcut")
	return value
}

func (n nativeWindowsIntegration) apply() error {
	if n.Desktop == "" {
		return errors.New("could not find the user desktop folder")
	}
	oldDesktop := n.previousDesktopShortcut()
	if err := n.removeAssociations(); err != nil {
		return err
	}
	removeOld := func(keep string) {
		if oldDesktop != "" && oldDesktop != keep && sameWindowsPath(filepath.Dir(oldDesktop), n.Desktop) {
			_ = os.Remove(oldDesktop)
		}
	}
	defer procSHChangeNotify.Call(shcneAssocChanged, 0, 0, 0)
	if n.Uninstall {
		removeOld("")
		return deleteRegistryTree(n.Root, n.appPath())
	}
	if err := n.Shortcut.create(); err != nil {
		return err
	}
	desktopShortcut := filepath.Join(n.Desktop, filepath.Base(n.Shortcut.Path))
	removeOld(desktopShortcut)
	link, err := os.ReadFile(n.Shortcut.Path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(desktopShortcut, link, 0o644); err != nil {
		return err
	}
	if err := setRegistryString(n.Root, n.appPath(), "DesktopShortcut", desktopShortcut); err != nil {
		return err
	}
	if len(n.Extensions) == 0 {
		return nil
	}
	classes := n.Base + `\Classes\` + n.ID
	capabilities := n.appPath() + `\Capabilities`
	icon := n.Shortcut.Icon + ",0"
	command := `"` + n.Shortcut.Target + `" ` + n.Shortcut.Args + ` -- "%1"`
	for _, value := range []struct{ path, name, value string }{
		{classes, "", n.Name + " Document"},
		{classes, "FriendlyTypeName", n.Name + " Document"},
		{classes + `\Application`, "ApplicationName", n.Name},
		{classes + `\Application`, "ApplicationIcon", icon},
		{classes + `\DefaultIcon`, "", icon},
		{classes + `\shell\open\command`, "", command},
		{capabilities, "ApplicationName", n.Name},
		{capabilities, "ApplicationDescription", n.Name + " (DynApp)"},
		{capabilities, "ApplicationIcon", icon},
	} {
		if err := setRegistryString(n.Root, value.path, value.name, value.value); err != nil {
			return err
		}
	}
	for _, extension := range n.Extensions {
		if err := setRegistryString(n.Root, capabilities+`\FileAssociations`, extension, n.ID); err != nil {
			return err
		}
		key, _, err := registry.CreateKey(n.Root, n.Base+`\Classes\`+extension+`\OpenWithProgids`, registry.SET_VALUE)
		if err != nil {
			return err
		}
		name, _ := comString(n.ID)
		status, _, _ := procRegSetValueExW.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0, 0 /* REG_NONE */, 0, 0)
		key.Close()
		if status != 0 {
			return fmt.Errorf("register %s for %s: %w", n.ID, extension, syscall.Errno(status))
		}
	}
	return setRegistryString(n.Root, n.Base+`\RegisteredApplications`, n.ID, capabilities)
}

func sameWindowsPath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
