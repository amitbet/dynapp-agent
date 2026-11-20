//go:build linux && !android

package shellagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Linux native apps run in dynapp-app-host, a separate GTK/WebKitGTK binary
// (cmd/dynapp-app-host). The agent stays pure Go so headless machines need no
// GUI libraries; it only writes the .desktop entry and starts the host.

const nativeHostExecutable = "dynapp-app-host"

func nativePlatformName() string { return "linux" }

// nativeHostPath finds the app host: $DYNAPP_APP_HOST, next to the agent,
// where the agent downloads it, then the usual install locations and PATH.
func nativeHostPath() string {
	if override := os.Getenv("DYNAPP_APP_HOST"); filepath.IsAbs(override) {
		if info, err := os.Stat(override); err == nil && info.Mode().IsRegular() {
			return override
		}
		return ""
	}
	candidates := []string{}
	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), nativeHostExecutable))
	}
	if dir := nativeAgentHostDir(); dir != "" {
		candidates = append(candidates, filepath.Join(dir, nativeHostExecutable))
	}
	candidates = append(candidates,
		"/usr/bin/"+nativeHostExecutable,
		"/usr/local/bin/"+nativeHostExecutable,
		"/usr/lib/dynapp-shell-agent/"+nativeHostExecutable,
	)
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", nativeHostExecutable))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	if found, err := exec.LookPath(nativeHostExecutable); err == nil {
		if absolute, err := filepath.Abs(found); err == nil {
			return absolute
		}
	}
	return ""
}

// sessionEnvironmentKeys are the variables a GUI program needs to reach the
// user's display and session bus. A systemd user service often starts before
// the desktop exports them, so they are also read from the user manager.
var sessionEnvironmentKeys = []string{
	"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
	"XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "XDG_SESSION_DESKTOP", "DESKTOP_SESSION", "XDG_DATA_DIRS",
	"GDK_BACKEND", "GTK_THEME", "LANG", "LC_ALL",
}

// nativeSessionEnv is the agent's environment completed with the graphical
// session's variables from `systemctl --user show-environment`.
func nativeSessionEnv() []string {
	environment := os.Environ()
	present := map[string]bool{}
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok && value != "" {
			present[key] = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Output()
	if err == nil {
		wanted := map[string]bool{}
		for _, key := range sessionEnvironmentKeys {
			wanted[key] = true
		}
		for _, line := range strings.Split(string(output), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || !wanted[key] || present[key] || value == "" {
				continue
			}
			environment = append(environment, key+"="+value)
			present[key] = true
		}
	}
	return environment
}

func environmentValue(environment []string, key string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		if name, value, ok := strings.Cut(environment[index], "="); ok && name == key {
			return value
		}
	}
	return ""
}

var nativeSupportCache struct {
	sync.Mutex
	at        time.Time
	supported bool
	reason    string
}

// nativeSupported needs the host binary, a graphical session, and the GTK and
// WebKitGTK libraries the host links against. `dynapp-app-host --check`
// proves the last two by opening the display.
func nativeSupported() (bool, string) {
	nativeSupportCache.Lock()
	defer nativeSupportCache.Unlock()
	if time.Since(nativeSupportCache.at) < 30*time.Second {
		return nativeSupportCache.supported, nativeSupportCache.reason
	}
	supported, reason := checkNativeSupport()
	nativeSupportCache.at, nativeSupportCache.supported, nativeSupportCache.reason = time.Now(), supported, reason
	return supported, reason
}

func checkNativeSupport() (bool, string) {
	host := nativeHostPath()
	if host == "" {
		return false, "install the DynApp app host (dynapp-app-host) to use native apps on Linux"
	}
	environment := nativeSessionEnv()
	if environmentValue(environment, "DISPLAY") == "" && environmentValue(environment, "WAYLAND_DISPLAY") == "" {
		return false, "native apps need a graphical desktop session"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, host, "--check")
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if strings.Contains(message, "error while loading shared libraries") {
			return false, "the DynApp app host needs GTK 3 and WebKitGTK 4.1 (libwebkit2gtk-4.1-0)"
		}
		if message == "" {
			message = err.Error()
		}
		return false, "the DynApp app host cannot start: " + truncateString(message, 200)
	}
	return true, ""
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// nativeEndpoint is the Unix socket installed apps connect to. sun_path is
// limited to 108 bytes, so a long state path falls back to the runtime dir.
func nativeEndpoint(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		var err error
		if stateDir, err = DefaultStateDir(); err != nil {
			return ""
		}
	}
	path := filepath.Join(stateDir, "native-host.sock")
	if len(path) > 100 {
		if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtimeDir) {
			path = filepath.Join(runtimeDir, "dynapp", "native-host.sock")
		} else {
			path = filepath.Join("/tmp", fmt.Sprintf("dynapp-%d", os.Getuid()), "native-host.sock")
		}
	}
	return path
}

func nativeListen(endpoint string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, err
	}
	if conn, err := net.DialTimeout("unix", endpoint, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return nil, errors.New("another agent is already serving native apps")
	}
	_ = os.Remove(endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// nativeVerifyPeer accepts only the app host binary, run by this user.
func nativeVerifyPeer(conn net.Conn, app NativeApp) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("native apps must connect over the Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if sockErr != nil || credentials == nil || credentials.Pid <= 0 {
		return errors.New("could not identify the connecting process")
	}
	if int(credentials.Uid) != os.Getuid() {
		return errors.New("the connecting process belongs to another user")
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", credentials.Pid))
	if err != nil {
		return fmt.Errorf("could not identify the connecting process: %w", err)
	}
	if replaced, ok := strings.CutSuffix(executable, " (deleted)"); ok {
		// The agent replaced the host binary while this app ran; the
		// process still runs the old file from the installed path.
		replaced = filepath.Clean(replaced)
		if replaced == filepath.Clean(app.Executable) || replaced == filepath.Clean(nativeHostPath()) {
			return nil
		}
		return errors.New("the connecting process runs a deleted program")
	}
	if sameLinuxFile(executable, app.Executable) || sameLinuxFile(executable, nativeHostPath()) {
		return nil
	}
	return fmt.Errorf("connecting process %s is not the DynApp app host", executable)
}

func sameLinuxFile(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

var linuxAppIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// nativeLinuxAppID is io.dynapp.app.<owner>.<slug> as a valid GApplication
// and D-Bus name. It names the .desktop file, the Wayland app_id and the X11
// WM_CLASS, so each app gets its own dock entry and single instance.
func nativeLinuxAppID(storeID string) string {
	owner, slug, _ := strings.Cut(strings.ToLower(storeID), "/")
	clean := func(value string) string {
		value = strings.Trim(linuxAppIDUnsafe.ReplaceAllString(value, "_"), "_")
		if value == "" {
			value = "app"
		}
		if value[0] >= '0' && value[0] <= '9' {
			value = "_" + value
		}
		return value
	}
	id := "io.dynapp.app." + clean(owner) + "." + clean(slug)
	if len(id) > 200 {
		id = id[:200]
	}
	return id
}

func nativeLinuxDirs(storeID string) (applications, appDir, mimePackages string, err error) {
	data, err := xdgDataHome()
	if err != nil {
		return "", "", "", err
	}
	owner, slug, _ := strings.Cut(storeID, "/")
	safe := func(value string) string {
		value = strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(value, "-"), ".-")
		if value == "" {
			value = "app"
		}
		return value
	}
	return filepath.Join(data, "applications"),
		filepath.Join(data, "dynapp", "native", safe(owner), safe(slug)),
		filepath.Join(data, "mime", "packages"), nil
}

// desktopExecArg quotes one argument for a .desktop Exec key: the Desktop
// Entry spec's quoting, then the string escaping of the key's value.
func desktopExecArg(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	if value != "" && !strings.ContainsAny(value, " \t\n\"'\\><~|&;$*?#()`=") {
		return value
	}
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(value) + `"`
	return strings.ReplaceAll(quoted, `\`, `\\`)
}

func desktopValue(value string) string {
	value = strings.NewReplacer("\\", "\\\\", "\n", " ", "\r", " ", "\t", " ").Replace(value)
	return strings.TrimSpace(value)
}

// nativeLinuxHostArgs is the host command line, without files.
func nativeLinuxHostArgs(app NativeApp, endpoint, icon string) []string {
	return []string{"--id", app.StoreID, "--app-id", nativeLinuxAppID(app.StoreID), "--socket", endpoint,
		"--url", app.URL, "--origin", app.Origin, "--name", app.Name, "--icon", icon}
}

var linuxMimeUnsafe = regexp.MustCompile(`[^a-z0-9.+-]+`)
var linuxExtensionSafe = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)
var linuxMimeTypeSafe = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// nativeLinuxMimeTypes returns the MimeType= list and, for extensions without
// a declared MIME type, a shared-mime-info package that defines one.
func nativeLinuxMimeTypes(storeID, name string, types []nativeDocumentType) ([]string, []byte) {
	mimeTypes := []string{}
	seen := map[string]bool{}
	add := func(mime string) {
		if !seen[mime] {
			seen[mime] = true
			mimeTypes = append(mimeTypes, mime)
		}
	}
	var definitions bytes.Buffer
	prefix := "application/x-dynapp-" + linuxMimeUnsafe.ReplaceAllString(strings.ToLower(strings.ReplaceAll(storeID, "/", "-")), "-")
	for _, doc := range types {
		declared := false
		for _, mime := range doc.MimeTypes {
			mime = strings.ToLower(strings.TrimSpace(mime))
			if linuxMimeTypeSafe.MatchString(mime) {
				add(mime)
				declared = true
			}
		}
		if declared {
			continue
		}
		for _, extension := range doc.Extensions {
			extension = strings.TrimPrefix(strings.TrimSpace(extension), ".")
			if !linuxExtensionSafe.MatchString(extension) {
				continue
			}
			mime := prefix + "-" + linuxMimeUnsafe.ReplaceAllString(strings.ToLower(extension), "-")
			if seen[mime] {
				continue
			}
			add(mime)
			comment := doc.Name
			if comment == "" {
				comment = name + " document"
			}
			fmt.Fprintf(&definitions, `<mime-type type="%s"><comment>%s</comment><glob pattern="*.%s"/></mime-type>`,
				mime, plistLikeEscape(comment), plistLikeEscape(extension))
		}
	}
	if definitions.Len() == 0 {
		return mimeTypes, nil
	}
	xml := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info">` + definitions.String() + "</mime-info>\n"
	return mimeTypes, []byte(xml)
}

func plistLikeEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(value)
}

// nativeDesktopEntry is the launcher entry for one app.
func nativeDesktopEntry(spec nativeInstallSpec, host, icon string, mimeTypes []string) []byte {
	app := NativeApp{StoreID: spec.StoreID, Name: spec.Name, Origin: spec.Origin, URL: spec.URL}
	args := []string{desktopExecArg(host)}
	for _, arg := range nativeLinuxHostArgs(app, spec.AgentEndpoint, icon) {
		args = append(args, desktopExecArg(arg))
	}
	args = append(args, "--", "%F")
	appID := nativeLinuxAppID(spec.StoreID)
	var entry bytes.Buffer
	entry.WriteString("[Desktop Entry]\nType=Application\nVersion=1.5\n")
	fmt.Fprintf(&entry, "Name=%s\n", desktopValue(spec.Name))
	fmt.Fprintf(&entry, "Comment=%s\n", desktopValue("DynApp from "+spec.Origin))
	fmt.Fprintf(&entry, "Exec=%s\n", strings.Join(args, " "))
	fmt.Fprintf(&entry, "TryExec=%s\n", desktopValue(host))
	fmt.Fprintf(&entry, "Icon=%s\n", desktopValue(icon))
	fmt.Fprintf(&entry, "StartupWMClass=%s\n", appID)
	entry.WriteString("StartupNotify=true\nTerminal=false\nDBusActivatable=false\nCategories=Utility;\n")
	if len(mimeTypes) > 0 {
		fmt.Fprintf(&entry, "MimeType=%s;\n", strings.Join(mimeTypes, ";"))
	}
	fmt.Fprintf(&entry, "X-DynApp-StoreID=%s\n", desktopValue(spec.StoreID))
	return entry.Bytes()
}

func refreshLinuxDesktopDatabases(applications, mimePackages string) {
	run := func(name string, args ...string) {
		if _, err := exec.LookPath(name); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, name, args...).Run()
	}
	run("update-mime-database", filepath.Dir(mimePackages))
	run("update-desktop-database", applications)
}

func nativeInstall(spec nativeInstallSpec) (nativeInstallResult, error) {
	host := nativeHostPath()
	if host == "" {
		return nativeInstallResult{}, errors.New("install the DynApp app host (dynapp-app-host) to use native apps on Linux")
	}
	applications, appDir, mimePackages, err := nativeLinuxDirs(spec.StoreID)
	if err != nil {
		return nativeInstallResult{}, err
	}
	for _, dir := range []string{applications, appDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nativeInstallResult{}, err
		}
	}
	icon := filepath.Join(appDir, "icon.png")
	if err := writeFileIfChanged(icon, spec.Icon); err != nil {
		return nativeInstallResult{}, err
	}
	if err := writeFileIfChanged(filepath.Join(appDir, "AGENTS.md"), appAgentNote(spec.note())); err != nil {
		return nativeInstallResult{}, err
	}
	appID := nativeLinuxAppID(spec.StoreID)
	mimeTypes, mimeXML := nativeLinuxMimeTypes(spec.StoreID, spec.Name, spec.DocumentTypes)
	mimePackage := filepath.Join(mimePackages, appID+".xml")
	if mimeXML != nil {
		if err := os.MkdirAll(mimePackages, 0o755); err != nil {
			return nativeInstallResult{}, err
		}
		if err := writeFileIfChanged(mimePackage, mimeXML); err != nil {
			return nativeInstallResult{}, err
		}
	} else {
		_ = os.Remove(mimePackage)
	}
	desktop := filepath.Join(applications, appID+".desktop")
	if err := writeFileIfChanged(desktop, nativeDesktopEntry(spec, host, icon, mimeTypes)); err != nil {
		return nativeInstallResult{}, err
	}
	refreshLinuxDesktopDatabases(applications, mimePackages)
	return nativeInstallResult{Path: desktop, Executable: host, BundleID: appID}, nil
}

func nativeUninstall(app NativeApp) error {
	applications, appDir, mimePackages, err := nativeLinuxDirs(app.StoreID)
	if err != nil {
		return err
	}
	appID := nativeLinuxAppID(app.StoreID)
	path := filepath.Clean(app.Path)
	if filepath.Dir(path) != applications || filepath.Base(path) != appID+".desktop" {
		return errors.New("the installed app is outside the applications folder")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(mimePackages, appID+".xml"))
	// Web data stays, like the browser path keeps site data.
	_ = os.Remove(filepath.Join(appDir, "icon.png"))
	_ = os.Remove(filepath.Join(appDir, "AGENTS.md"))
	refreshLinuxDesktopDatabases(applications, mimePackages)
	return nil
}

func nativeLaunch(app NativeApp, endpoint string) error {
	host := nativeHostPath()
	if host == "" {
		return errors.New("install the DynApp app host (dynapp-app-host) to open native apps")
	}
	_, appDir, _, err := nativeLinuxDirs(app.StoreID)
	if err != nil {
		return err
	}
	command := exec.Command(host, nativeLinuxHostArgs(app, endpoint, filepath.Join(appDir, "icon.png"))...)
	command.Env = nativeSessionEnv()
	command.Dir = appDir
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("open %s: %w", app.Name, err)
	}
	go func() {
		if err := command.Wait(); err != nil {
			log.Printf("DynApp Shell agent: app host for %s exited: %v", app.StoreID, err)
		}
	}()
	return nil
}

// writeNativeAppNote keeps AGENTS.md in the app's folder current.
func writeNativeAppNote(app NativeApp, note appNote) {
	_, appDir, _, err := nativeLinuxDirs(app.StoreID)
	if err != nil {
		return
	}
	if info, err := os.Stat(appDir); err == nil && info.IsDir() {
		_ = writeFileIfChanged(filepath.Join(appDir, "AGENTS.md"), appAgentNote(note))
	}
}
