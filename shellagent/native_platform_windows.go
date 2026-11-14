//go:build windows

package shellagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/amitbet/dynapp-agent/shellagent/desktop"
	"github.com/amitbet/dynapp-agent/shellagent/winhost"
	"golang.org/x/sys/windows"
)

func nativePlatformName() string { return "windows" }

// Windows native apps stay behind an explicit opt-in until they have been
// verified on real machines (Server.nativeAvailable).
func nativeRequiresOptIn() bool { return true }

func nativeSupported() (bool, string) {
	if _, err := webView2RuntimeVersion(); err != nil {
		return false, "the Microsoft Edge WebView2 Runtime is not installed"
	}
	return true, ""
}

// webView2RuntimeVersion reads the Evergreen runtime registration.
func webView2RuntimeVersion() (string, error) {
	const client = `SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	for _, location := range []struct {
		root windows.Handle
		path string
	}{
		{windows.HKEY_LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`},
		{windows.HKEY_LOCAL_MACHINE, client},
		{windows.HKEY_CURRENT_USER, client},
	} {
		if version, err := readRegistryString(location.root, location.path, "pv"); err == nil && version != "" && version != "0.0.0.0" {
			return version, nil
		}
	}
	return "", errors.New("WebView2 runtime not found")
}

func readRegistryString(root windows.Handle, path, name string) (string, error) {
	pathPtr, _ := windows.UTF16PtrFromString(path)
	var key windows.Handle
	if err := windows.RegOpenKeyEx(root, pathPtr, 0, windows.KEY_READ|windows.KEY_WOW64_64KEY, &key); err != nil {
		return "", err
	}
	defer windows.RegCloseKey(key)
	namePtr, _ := windows.UTF16PtrFromString(name)
	buffer := make([]uint16, 256)
	size := uint32(len(buffer) * 2)
	var kind uint32
	if err := windows.RegQueryValueEx(key, namePtr, nil, &kind, (*byte)(unsafe.Pointer(&buffer[0])), &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size/2]), nil
}

func runningAsLocalSystem() bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

var pipeNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// nativeEndpoint is the named pipe installed apps connect to: one per user
// for a per-user agent, one per machine for the LocalSystem service.
func nativeEndpoint(string) string {
	if runningAsLocalSystem() {
		return `\\.\pipe\dynapp-native-host`
	}
	name := os.Getenv("USERNAME")
	if name == "" {
		name = "user"
	}
	return `\\.\pipe\dynapp-native-host-` + pipeNameUnsafe.ReplaceAllString(strings.ToLower(name), "-")
}

func nativeListen(endpoint string) (net.Listener, error) {
	descriptor := "D:P(A;;GA;;;SY)(A;;GRGW;;;IU)"
	if !runningAsLocalSystem() {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return nil, err
		}
		descriptor = "D:P(A;;GA;;;SY)(A;;GA;;;" + user.User.Sid.String() + ")"
	}
	return winio.ListenPipe(endpoint, &winio.PipeConfig{SecurityDescriptor: descriptor, InputBufferSize: 256 * 1024, OutputBufferSize: 256 * 1024})
}

var procGetNamedPipeClientProcessID = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")

// nativeVerifyPeer accepts only the agent executable running as an app host
// (`dynapp-shell-agent.exe app --id …`).
func nativeVerifyPeer(conn net.Conn, app NativeApp) error {
	withHandle, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("native apps must connect over the named pipe")
	}
	var pid uint32
	if result, _, callErr := procGetNamedPipeClientProcessID.Call(withHandle.Fd(), uintptr(unsafe.Pointer(&pid))); result == 0 {
		return fmt.Errorf("identify the connecting process: %w", callErr)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return err
	}
	peer := windows.UTF16ToString(buffer[:size])
	executable, err := installedAgentExecutable()
	if err != nil {
		return err
	}
	// An update renames the running image to <exe>.old-<pid>; a host started
	// before the update is still the agent.
	if !strings.EqualFold(filepath.Clean(peer), filepath.Clean(executable)) &&
		!(strings.EqualFold(filepath.Dir(peer), filepath.Dir(executable)) && strings.HasPrefix(strings.ToLower(filepath.Base(peer)), strings.ToLower(filepath.Base(executable))+".old-")) {
		return fmt.Errorf("connecting process %s is not the DynApp agent", peer)
	}
	return nil
}

func installedAgentExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return executable, nil
}

// nativeAppUserModelID gives each app its own taskbar identity; the running
// host sets the same value.
func nativeAppUserModelID(storeID string) string { return winhost.AppUserModelID(storeID) }

var windowsNameUnsafe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]+`)

func nativeShortcutName(name string) string {
	name = strings.TrimRight(strings.TrimSpace(windowsNameUnsafe.ReplaceAllString(name, " ")), ". ")
	if len(name) > 80 {
		name = strings.TrimSpace(name[:80])
	}
	if name == "" {
		name = "DynApp"
	}
	return name
}

// nativeUserDirs returns the signed-in user's LocalAppData and AppData, also
// when the agent runs as the LocalSystem service.
func nativeUserDirs() (*desktop.UserEnvironment, string, string, error) {
	user, err := desktop.CurrentUser()
	if err != nil {
		return nil, "", "", err
	}
	local, roaming := user.Getenv("LOCALAPPDATA"), user.Getenv("APPDATA")
	if local == "" || roaming == "" {
		user.Close()
		return nil, "", "", errors.New("could not find the signed-in user's profile folders")
	}
	return user, local, roaming, nil
}

func nativeAppDir(local, storeID string) string {
	owner, slug, _ := strings.Cut(storeID, "/")
	return filepath.Join(local, "DynApp", "Apps", nativeShortcutName(owner), nativeShortcutName(slug))
}

// pngToICO wraps a PNG in a single-image .ico, which Windows Vista and later
// read directly.
func pngToICO(png []byte) []byte {
	var buffer bytes.Buffer
	_ = binary.Write(&buffer, binary.LittleEndian, [3]uint16{0, 1, 1})
	width, height := byte(0), byte(0) // 0 means 256 or larger
	if len(png) >= 24 {
		if w := binary.BigEndian.Uint32(png[16:20]); w < 256 {
			width = byte(w)
		}
		if h := binary.BigEndian.Uint32(png[20:24]); h < 256 {
			height = byte(h)
		}
	}
	buffer.Write([]byte{width, height, 0, 0})
	_ = binary.Write(&buffer, binary.LittleEndian, [2]uint16{1, 32})
	_ = binary.Write(&buffer, binary.LittleEndian, [2]uint32{uint32(len(png)), 22})
	buffer.Write(png)
	return buffer.Bytes()
}

// nativeHostArgs is the command line a shortcut or launch uses.
func nativeHostArgs(app NativeApp, endpoint, icon string) []string {
	return []string{"app", "--id", app.StoreID, "--pipe", endpoint, "--url", app.URL, "--origin", app.Origin, "--name", app.Name, "--icon", icon}
}

func joinWindowsArgs(args []string) string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = windows.EscapeArg(arg)
	}
	return strings.Join(quoted, " ")
}

func nativeInstall(spec nativeInstallSpec) (nativeInstallResult, error) {
	user, local, roaming, err := nativeUserDirs()
	if err != nil {
		return nativeInstallResult{}, err
	}
	defer user.Close()
	executable, err := installedAgentExecutable()
	if err != nil {
		return nativeInstallResult{}, err
	}
	dir := nativeAppDir(local, spec.StoreID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nativeInstallResult{}, err
	}
	icon := filepath.Join(dir, "icon.ico")
	if err := os.WriteFile(icon, pngToICO(spec.Icon), 0o644); err != nil {
		return nativeInstallResult{}, err
	}
	if err := writeFileIfChanged(filepath.Join(dir, "AGENTS.md"), appAgentNote(spec.note())); err != nil {
		return nativeInstallResult{}, err
	}
	shortcut := filepath.Join(roaming, "Microsoft", "Windows", "Start Menu", "Programs", "DynApp", nativeShortcutName(spec.Name)+".lnk")
	app := NativeApp{StoreID: spec.StoreID, Name: spec.Name, Origin: spec.Origin, URL: spec.URL}
	integration := nativeWindowsIntegration{
		Base: "Software", ID: nativeWindowsRegistrationID(spec.StoreID), Name: spec.Name,
		Extensions: nativeWindowsExtensions(spec.DocumentTypes),
		Shortcut: windowsShortcut{
			Path: shortcut, Target: executable, Args: joinWindowsArgs(nativeHostArgs(app, spec.AgentEndpoint, icon)),
			Icon: icon, AppID: nativeAppUserModelID(spec.StoreID), Description: nativeShortcutDescription(spec),
		},
	}
	if err := applyNativeWindowsIntegration(user, integration); err != nil {
		return nativeInstallResult{}, fmt.Errorf("install Windows app integration: %w", err)
	}
	// A renamed app leaves its previous shortcut behind; remove it.
	programs := filepath.Dir(shortcut)
	if previous, ok := previousNativeShortcut(programs, spec.StoreID, shortcut); ok {
		_ = os.Remove(previous)
	}
	return nativeInstallResult{Path: shortcut, Executable: executable, BundleID: nativeAppUserModelID(spec.StoreID)}, nil
}

// previousNativeShortcut finds another DynApp shortcut for the same app by
// looking for its --id argument in the .lnk.
func previousNativeShortcut(programs, storeID, current string) (string, bool) {
	entries, err := os.ReadDir(programs)
	if err != nil {
		return "", false
	}
	needle := utf16Bytes(storeID)
	for _, entry := range entries {
		path := filepath.Join(programs, entry.Name())
		if strings.EqualFold(path, current) || !strings.HasSuffix(strings.ToLower(entry.Name()), ".lnk") {
			continue
		}
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, needle) {
			return path, true
		}
	}
	return "", false
}

func utf16Bytes(value string) []byte {
	encoded := utf16.Encode([]rune(value))
	raw := make([]byte, len(encoded)*2)
	for index, unit := range encoded {
		binary.LittleEndian.PutUint16(raw[index*2:], unit)
	}
	return raw
}

// nativeUninstall removes both shortcuts, file registrations, and the icon. WebView2 data stays, like
// the browser path keeps site data.
func nativeUninstall(app NativeApp) error {
	user, local, roaming, err := nativeUserDirs()
	if err != nil {
		return err
	}
	defer user.Close()
	programs := filepath.Join(roaming, "Microsoft", "Windows", "Start Menu", "Programs", "DynApp")
	path := filepath.Clean(app.Path)
	if !strings.EqualFold(filepath.Dir(path), programs) || !strings.HasSuffix(strings.ToLower(path), ".lnk") {
		return errors.New("the installed app is outside the DynApp Start menu folder")
	}
	integration := nativeWindowsIntegration{Base: "Software", ID: nativeWindowsRegistrationID(app.StoreID), Uninstall: true}
	if err := applyNativeWindowsIntegration(user, integration); err != nil {
		return fmt.Errorf("remove Windows app integration: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(nativeAppDir(local, app.StoreID), "icon.ico"))
	_ = os.Remove(filepath.Join(nativeAppDir(local, app.StoreID), "AGENTS.md"))
	return nil
}

func nativeLaunch(app NativeApp, endpoint string) error {
	user, local, _, err := nativeUserDirs()
	if err != nil {
		return err
	}
	defer user.Close()
	executable, err := installedAgentExecutable()
	if err != nil {
		return err
	}
	icon := filepath.Join(nativeAppDir(local, app.StoreID), "icon.ico")
	return user.StartGUI(executable, nativeHostArgs(app, endpoint, icon), filepath.Dir(executable))
}

// applyNativeWindowsIntegration runs the integration as the signed-in user:
// their registry hive, their desktop folder, files owned by them.
func applyNativeWindowsIntegration(user *desktop.UserEnvironment, integration nativeWindowsIntegration) error {
	desktopDir, err := user.KnownFolderPath(windows.FOLDERID_Desktop)
	if err != nil {
		return fmt.Errorf("find the user desktop folder: %w", err)
	}
	integration.Desktop = desktopDir
	return user.AsUser(func() error {
		root, err := currentUserRegistry()
		if err != nil {
			return err
		}
		defer root.Close()
		integration.Root = root
		return integration.apply()
	})
}

// Stable, owner-scoped identity, independent of an app's display name.
func nativeWindowsRegistrationID(storeID string) string {
	return fmt.Sprintf("DynApp.Native.%x", sha256.Sum256([]byte(storeID)))
}

var nativeWindowsExtension = regexp.MustCompile(`^[a-z0-9][a-z0-9_+-]{0,63}$`)

func nativeWindowsExtensions(types []nativeDocumentType) []string {
	extensions := []string{}
	seen := map[string]bool{}
	for _, kind := range types {
		for _, raw := range kind.Extensions {
			ext := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "."))
			if nativeWindowsExtension.MatchString(ext) && !seen[ext] {
				extensions = append(extensions, "."+ext)
				seen[ext] = true
			}
		}
	}
	return extensions
}

// codingAgentsDir holds installed-apps.md and the local skill copy, in the
// signed-in user's LocalAppData.
func codingAgentsDir() (string, error) {
	user, local, _, err := nativeUserDirs()
	if err != nil {
		return "", err
	}
	user.Close()
	return filepath.Join(local, "DynApp", "agents"), nil
}

// legacyAppNotesDir is where agents 0.1.59-0.1.61 wrote per-app notes.
func legacyAppNotesDir() (string, error) {
	user, local, _, err := nativeUserDirs()
	if err != nil {
		return "", err
	}
	user.Close()
	return filepath.Join(local, "DynApp", "Apps"), nil
}

// writeNativeAppNote keeps AGENTS.md in the app's folder current.
func writeNativeAppNote(app NativeApp, note appNote) {
	user, local, _, err := nativeUserDirs()
	if err != nil {
		return
	}
	user.Close()
	if dir := nativeAppDir(local, app.StoreID); dirExists(dir) {
		_ = writeFileIfChanged(filepath.Join(dir, "AGENTS.md"), appAgentNote(note))
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// codingAgentHomes is the signed-in user's profile and CODEX_HOME, also when
// the agent runs as the LocalSystem service.
func codingAgentHomes() (string, string, error) {
	user, err := desktop.CurrentUser()
	if err != nil {
		return "", "", err
	}
	defer user.Close()
	home := user.Getenv("USERPROFILE")
	if home == "" {
		return "", "", errors.New("could not find the signed-in user's profile folder")
	}
	return home, user.Getenv("CODEX_HOME"), nil
}

// nativeShortcutDescription is the Start menu tooltip. The store id tells a
// coding agent that reads the shortcut which DynApp this is.
func nativeShortcutDescription(spec nativeInstallSpec) string {
	return spec.Name + " (DynApp " + spec.StoreID + ")"
}
