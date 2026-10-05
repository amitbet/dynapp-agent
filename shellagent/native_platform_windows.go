//go:build windows

package shellagent

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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

// shortcutScript creates a .lnk whose System.AppUserModel.ID matches the
// running host, so pinning and taskbar grouping work per app. Values arrive
// through environment variables to avoid quoting.
const shortcutScript = `$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Text;
[ComImport, Guid("000214F9-0000-0000-C000-000000000046"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
interface IShellLinkW {
  void GetPath([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder file, int max, IntPtr data, uint flags);
  void GetIDList(out IntPtr list); void SetIDList(IntPtr list);
  void GetDescription([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder name, int max);
  void SetDescription([MarshalAs(UnmanagedType.LPWStr)] string name);
  void GetWorkingDirectory([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder dir, int max);
  void SetWorkingDirectory([MarshalAs(UnmanagedType.LPWStr)] string dir);
  void GetArguments([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder args, int max);
  void SetArguments([MarshalAs(UnmanagedType.LPWStr)] string args);
  void GetHotkey(out short key); void SetHotkey(short key);
  void GetShowCmd(out int cmd); void SetShowCmd(int cmd);
  void GetIconLocation([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder path, int max, out int index);
  void SetIconLocation([MarshalAs(UnmanagedType.LPWStr)] string path, int index);
  void SetRelativePath([MarshalAs(UnmanagedType.LPWStr)] string path, uint reserved);
  void Resolve(IntPtr hwnd, uint flags);
  void SetPath([MarshalAs(UnmanagedType.LPWStr)] string file);
}
[ComImport, Guid("886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
interface IPropertyStore {
  void GetCount(out uint count); void GetAt(uint index, out PropertyKey key);
  void GetValue(ref PropertyKey key, out PropVariant value); void SetValue(ref PropertyKey key, ref PropVariant value);
  void Commit();
}
[StructLayout(LayoutKind.Sequential, Pack = 4)] public struct PropertyKey { public Guid fmtid; public uint pid; }
[StructLayout(LayoutKind.Explicit)] public struct PropVariant { [FieldOffset(0)] public ushort vt; [FieldOffset(8)] public IntPtr value; }
[ComImport, Guid("00021401-0000-0000-C000-000000000046")] class CShellLink {}
public static class DynAppShortcut {
  public static void Create(string path, string target, string args, string icon, string appId, string description) {
    var link = (IShellLinkW)new CShellLink();
    link.SetPath(target);
    link.SetArguments(args);
    link.SetIconLocation(icon, 0);
    link.SetDescription(description);
    link.SetWorkingDirectory(System.IO.Path.GetDirectoryName(target));
    link.SetShowCmd(7);
    var key = new PropertyKey { fmtid = new Guid("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3"), pid = 5 };
    var value = new PropVariant { vt = 31, value = Marshal.StringToCoTaskMemUni(appId) };
    try {
      var store = (IPropertyStore)link;
      store.SetValue(ref key, ref value);
      store.Commit();
    } finally {
      Marshal.FreeCoTaskMem(value.value);
    }
    ((IPersistFile)link).Save(path, true);
  }
}
'@
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $env:DYNAPP_LNK_PATH) | Out-Null
[DynAppShortcut]::Create($env:DYNAPP_LNK_PATH, $env:DYNAPP_LNK_TARGET, $env:DYNAPP_LNK_ARGS, $env:DYNAPP_LNK_ICON, $env:DYNAPP_LNK_APPID, $env:DYNAPP_LNK_DESCRIPTION)
`

func encodedPowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	raw := make([]byte, len(encoded)*2)
	for index, unit := range encoded {
		binary.LittleEndian.PutUint16(raw[index*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(raw)
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
	shortcut := filepath.Join(roaming, "Microsoft", "Windows", "Start Menu", "Programs", "DynApp", nativeShortcutName(spec.Name)+".lnk")
	app := NativeApp{StoreID: spec.StoreID, Name: spec.Name, Origin: spec.Origin, URL: spec.URL}
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	command := user.Command(nil, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodedPowerShell(shortcutScript+nativeWindowsIntegrationScript))
	command.Env = append(user.Environ(),
		"DYNAPP_LNK_PATH="+shortcut, "DYNAPP_LNK_TARGET="+executable,
		"DYNAPP_LNK_ARGS="+joinWindowsArgs(nativeHostArgs(app, spec.AgentEndpoint, icon)),
		"DYNAPP_LNK_ICON="+icon, "DYNAPP_LNK_APPID="+nativeAppUserModelID(spec.StoreID),
		"DYNAPP_LNK_DESCRIPTION="+spec.Name+" (DynApp)",
		"DYNAPP_NATIVE_ID="+nativeWindowsRegistrationID(spec.StoreID),
		"DYNAPP_NATIVE_NAME="+spec.Name,
		"DYNAPP_NATIVE_EXTENSIONS="+nativeWindowsExtensionsJSON(spec.DocumentTypes),
		"DYNAPP_NATIVE_MODE=install",
	)
	if output, err := command.CombinedOutput(); err != nil {
		return nativeInstallResult{}, fmt.Errorf("install Windows app integration: %w: %s", err, strings.TrimSpace(string(output)))
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
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	command := user.Command(nil, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodedPowerShell(nativeWindowsIntegrationScript))
	command.Env = append(user.Environ(), "DYNAPP_NATIVE_MODE=uninstall", "DYNAPP_NATIVE_ID="+nativeWindowsRegistrationID(app.StoreID))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove Windows app integration: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(nativeAppDir(local, app.StoreID), "icon.ico"))
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

//go:embed native_windows_integration.ps1
var nativeWindowsIntegrationScript string

// Stable, owner-scoped identity, independent of an app's display name.
func nativeWindowsRegistrationID(storeID string) string {
	return fmt.Sprintf("DynApp.Native.%x", sha256.Sum256([]byte(storeID)))
}

var nativeWindowsExtension = regexp.MustCompile(`^[a-z0-9][a-z0-9_+-]{0,63}$`)

func nativeWindowsExtensionsJSON(types []nativeDocumentType) string {
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
	data, _ := json.Marshal(extensions)
	return string(data)
}
