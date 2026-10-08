//go:build android

package shellagent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/fs"
	"golang.org/x/sys/unix"
)

// On Android the agent runs as a child process of the DynApp app, inside the
// app's sandbox UID. The app owns the WebViews, launcher shortcuts, and
// permission prompts; the agent asks it over the platform channel to pin,
// remove, and open apps (docs/native-host.md, "Android").

func nativePlatformName() string { return "android" }

// Android loopback is reachable by every app on the device, not just this
// user's programs.
const loopbackSharedAcrossApps = true

// Android has no xdg-open, xclip, or Secret Service. The app performs these
// with Intents, ClipboardManager, and the Android Keystore.
func init() {
	platformOpenPath = func(ctx context.Context, path string, reveal bool) error {
		_, err := callNativePlatform(ctx, "openPath", map[string]any{"path": path, "reveal": reveal})
		return err
	}
	fs.PlatformOpen = func(path string) error { return platformOpenPath(context.Background(), path, false) }
	platformWriteClipboardText = func(text string) error {
		_, err := callNativePlatform(context.Background(), "clipboardWriteText", map[string]any{"text": text})
		return err
	}
	secretStoreSet = func(service, name, value string) error {
		_, err := callNativePlatform(context.Background(), "secretSet", map[string]any{"service": service, "name": name, "value": value})
		return err
	}
	secretStoreDelete = func(service, name string) error {
		_, err := callNativePlatform(context.Background(), "secretDelete", map[string]any{"service": service, "name": name})
		return err
	}
}

func nativeRequiresOptIn() bool { return false }

func nativeSupported() (bool, string) { return true, "" }

func nativePlatformUsesHostChannel() bool { return true }

func nativeEndpoint(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		var err error
		if stateDir, err = DefaultStateDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(stateDir, "native-host.sock")
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

// appProcessPeer accepts only the DynApp app process that started this agent.
// Android gives every APK its own UID, so the UID check excludes other apps;
// the parent check excludes programs the agent itself runs for apps
// (fs.exec), which share the UID. The app binds each page connection to the
// origin of the WebView that opened it.
func appProcessPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("native apps must connect over the Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if sockErr != nil || cred == nil {
		return errors.New("could not identify the connecting process")
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("connecting uid %d is not the DynApp app", cred.Uid)
	}
	if int(cred.Pid) != os.Getppid() {
		return fmt.Errorf("connecting process %d is not the DynApp app", cred.Pid)
	}
	return nil
}

func nativeVerifyPeer(conn net.Conn, _ NativeApp) error { return appProcessPeer(conn) }

func nativeVerifyPlatformPeer(conn net.Conn) error { return appProcessPeer(conn) }

func androidShortcutPath(storeID string) string { return "android:shortcut/" + storeID }

func nativeInstall(spec nativeInstallSpec) (nativeInstallResult, error) {
	types := make([]map[string]any, 0, len(spec.DocumentTypes))
	for _, documentType := range spec.DocumentTypes {
		types = append(types, map[string]any{
			"name": documentType.Name, "extensions": documentType.Extensions, "mimeTypes": documentType.MimeTypes,
		})
	}
	if _, err := callNativePlatform(context.Background(), "install", map[string]any{
		"storeId": spec.StoreID, "name": spec.Name, "origin": spec.Origin, "url": spec.URL,
		"iconPng": base64.StdEncoding.EncodeToString(spec.Icon), "documentTypes": types,
	}); err != nil {
		return nativeInstallResult{}, err
	}
	return nativeInstallResult{Path: androidShortcutPath(spec.StoreID)}, nil
}

func nativeUninstall(app NativeApp) error {
	if app.Path == nativeCatalogPath {
		return errors.New("the catalog is part of the DynApp app and cannot be uninstalled")
	}
	_, err := callNativePlatform(context.Background(), "uninstall", map[string]any{"storeId": app.StoreID, "name": app.Name})
	return err
}

func nativeLaunch(app NativeApp, _ string) error {
	_, err := callNativePlatform(context.Background(), "launch", map[string]any{
		"storeId": app.StoreID, "name": app.Name, "origin": app.Origin, "url": app.URL,
	})
	return err
}
