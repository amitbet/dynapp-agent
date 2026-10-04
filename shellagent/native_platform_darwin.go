//go:build darwin

package shellagent

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// nativeHostBinary is the prebuilt universal Swift host
// (nativehost/darwin/README.md). It is committed rather than built in CI so
// its bytes, and therefore the privacy grants of installed apps, only change
// when the host itself is deliberately rebuilt.
//
//go:embed nativehost/darwin/dynapp-app-host
var nativeHostBinary []byte

// lsregisterEnabled lets tests build bundles without registering them.
var lsregisterEnabled = true

func lsregister(args ...string) {
	if lsregisterEnabled {
		_ = exec.Command(lsregisterPath, args...).Run()
	}
}

const (
	nativeHostExecutable = "dynapp-app-host"
	lsregisterPath       = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
)

func nativePlatformName() string { return "macos" }

func nativeRequiresOptIn() bool { return false }

func nativeSupported() (bool, string) {
	if len(nativeHostBinary) < 4096 {
		return false, "this agent build does not include the macOS app host"
	}
	for _, tool := range []string{"/usr/bin/codesign", "/usr/bin/sips", "/usr/bin/iconutil", "/usr/bin/open"} {
		if _, err := os.Stat(tool); err != nil {
			return false, filepath.Base(tool) + " is not available"
		}
	}
	return true, ""
}

// nativeEndpoint is the Unix socket installed apps connect to. sun_path is
// limited to 104 bytes, so a long state path falls back to a per-user
// directory under /tmp.
func nativeEndpoint(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		var err error
		if stateDir, err = DefaultStateDir(); err != nil {
			return ""
		}
	}
	path := filepath.Join(stateDir, "native-host.sock")
	if len(path) > 100 {
		path = filepath.Join("/tmp", fmt.Sprintf("dynapp-%d", os.Getuid()), "native-host.sock")
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

var nativePeerCache sync.Map // pid → verifiedPeer

type verifiedPeer struct {
	executable string
	cdhash     string
	at         time.Time
}

// nativeVerifyPeer accepts only the host executable inside the bundle the
// agent installed for this app, with the signature recorded at install.
func nativeVerifyPeer(conn net.Conn, app NativeApp) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("native apps must connect over the Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	pid := 0
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		pid, sockErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return err
	}
	if sockErr != nil || pid <= 0 {
		return errors.New("could not identify the connecting process")
	}
	executable, err := processExecutablePath(pid)
	if err != nil {
		return err
	}
	if !sameFile(executable, app.Executable) {
		return fmt.Errorf("connecting process %s is not %s", executable, app.Executable)
	}
	if cached, ok := nativePeerCache.Load(pid); ok {
		peer := cached.(verifiedPeer)
		if peer.executable == app.Executable && peer.cdhash == app.CDHash && time.Since(peer.at) < 10*time.Minute {
			return nil
		}
	}
	cdhash, err := bundleCDHash(app.Path)
	if err != nil {
		return err
	}
	if app.CDHash == "" || !strings.EqualFold(cdhash, app.CDHash) {
		return errors.New("the app bundle signature changed after it was installed")
	}
	nativePeerCache.Store(pid, verifiedPeer{executable: app.Executable, cdhash: app.CDHash, at: time.Now()})
	return nil
}

func sameFile(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

var cdhashPattern = regexp.MustCompile(`(?m)^CDHash=([0-9a-f]{40})$`)

func bundleCDHash(bundle string) (string, error) {
	output, err := exec.Command("/usr/bin/codesign", "-dvvv", bundle).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read the app signature: %w: %s", err, strings.TrimSpace(string(output)))
	}
	match := cdhashPattern.FindSubmatch(output)
	if match == nil {
		return "", errors.New("the app bundle has no code directory hash")
	}
	return string(match[1]), nil
}

func nativeInstallRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Applications", "DynApp"), nil
}

var bundleIDUnsafe = regexp.MustCompile(`[^a-z0-9.-]+`)

// nativeBundleID is io.dynapp.app.<owner>.<slug> in the character set macOS
// accepts for CFBundleIdentifier.
func nativeBundleID(storeID string) string {
	owner, slug, _ := strings.Cut(strings.ToLower(storeID), "/")
	clean := func(value string) string {
		value = bundleIDUnsafe.ReplaceAllString(strings.ReplaceAll(value, "_", "-"), "-")
		return strings.Trim(value, ".-")
	}
	return "io.dynapp.app." + clean(owner) + "." + clean(slug)
}

var fileNameUnsafe = regexp.MustCompile(`[/:\x00-\x1f]+`)

func nativeBundleName(name string) string {
	name = strings.TrimSpace(fileNameUnsafe.ReplaceAllString(name, " "))
	name = strings.TrimLeft(name, ".")
	if len(name) > 80 {
		name = strings.TrimSpace(name[:80])
	}
	if name == "" {
		name = "DynApp"
	}
	return name
}

func plistEscape(value string) string {
	var buffer bytes.Buffer
	for _, r := range value {
		switch r {
		case '&':
			buffer.WriteString("&amp;")
		case '<':
			buffer.WriteString("&lt;")
		case '>':
			buffer.WriteString("&gt;")
		case '"':
			buffer.WriteString("&quot;")
		default:
			if r >= 0x20 || r == '\t' || r == '\n' {
				buffer.WriteRune(r)
			}
		}
	}
	return buffer.String()
}

// nativeInfoPlist is deterministic and carries no version that changes, so
// regenerating an unchanged app yields a byte-identical bundle.
func nativeInfoPlist(spec nativeInstallSpec, bundleID string) []byte {
	name := plistEscape(spec.Name)
	entries := [][2]string{
		{"CFBundleDevelopmentRegion", "en"},
		{"CFBundleDisplayName", name},
		{"CFBundleExecutable", nativeHostExecutable},
		{"CFBundleIconFile", "AppIcon"},
		{"CFBundleIdentifier", plistEscape(bundleID)},
		{"CFBundleInfoDictionaryVersion", "6.0"},
		{"CFBundleName", name},
		{"CFBundlePackageType", "APPL"},
		{"CFBundleShortVersionString", "1.0"},
		{"CFBundleVersion", "1"},
		{"DynAppAgentSocket", plistEscape(spec.AgentEndpoint)},
		{"DynAppOrigin", plistEscape(spec.Origin)},
		{"DynAppStoreID", plistEscape(spec.StoreID)},
		{"DynAppURL", plistEscape(spec.URL)},
		{"LSApplicationCategoryType", "public.app-category.productivity"},
		{"LSMinimumSystemVersion", "13.0"},
		{"NSCameraUsageDescription", name + " uses the camera when the app asks for it."},
		{"NSMicrophoneUsageDescription", name + " uses the microphone when the app asks for it."},
	}
	var buffer bytes.Buffer
	buffer.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	for _, entry := range entries {
		fmt.Fprintf(&buffer, "\t<key>%s</key>\n\t<string>%s</string>\n", entry[0], entry[1])
	}
	if len(spec.DocumentTypes) > 0 {
		buffer.WriteString("<key>CFBundleDocumentTypes</key><array>\n")
		for _, doc := range spec.DocumentTypes {
			if len(doc.Extensions) == 0 {
				continue
			}
			role := doc.Role
			if role != "Editor" && role != "Viewer" && role != "Shell" {
				role = "Viewer"
			}
			rank := doc.Rank
			if rank != "Owner" && rank != "Default" && rank != "None" {
				rank = "Alternate"
			}
			fmt.Fprintf(&buffer, "<dict><key>CFBundleTypeName</key><string>%s</string><key>CFBundleTypeRole</key><string>%s</string><key>LSHandlerRank</key><string>%s</string><key>CFBundleTypeExtensions</key><array>", plistEscape(doc.Name), role, rank)
			for _, ext := range doc.Extensions {
				fmt.Fprintf(&buffer, "<string>%s</string>", plistEscape(strings.TrimPrefix(ext, ".")))
			}
			buffer.WriteString("</array><key>CFBundleTypeMIMETypes</key><array>")
			for _, mime := range doc.MimeTypes {
				fmt.Fprintf(&buffer, "<string>%s</string>", plistEscape(mime))
			}
			buffer.WriteString("</array></dict>\n")
		}
		buffer.WriteString("</array>\n")
	}
	buffer.WriteString("\t<key>NSHighResolutionCapable</key>\n\t<true/>\n</dict>\n</plist>\n")
	return buffer.Bytes()
}

// nativeIcns resizes and masks the app PNG before encoding its macOS icon.
func nativeIcns(png []byte) ([]byte, error) {
	work, err := os.MkdirTemp("", "dynapp-icon-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	source := filepath.Join(work, "source.png")
	if err := os.WriteFile(source, png, 0o600); err != nil {
		return nil, err
	}
	iconset := filepath.Join(work, "AppIcon.iconset")
	if err := os.Mkdir(iconset, 0o700); err != nil {
		return nil, err
	}
	sizes := map[string]int{
		"icon_16x16.png": 16, "icon_16x16@2x.png": 32, "icon_32x32.png": 32, "icon_32x32@2x.png": 64,
		"icon_128x128.png": 128, "icon_128x128@2x.png": 256, "icon_256x256.png": 256,
		"icon_256x256@2x.png": 512, "icon_512x512.png": 512,
	}
	for name, size := range sizes {
		output, err := exec.Command("/usr/bin/sips", "-s", "format", "png", "-z", fmt.Sprint(size), fmt.Sprint(size), source, "--out", filepath.Join(iconset, name)).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("resize the app icon: %w: %s", err, strings.TrimSpace(string(output)))
		}
		path := filepath.Join(iconset, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		masked, err := maskNativeMacIcon(data)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, masked, 0o600); err != nil {
			return nil, err
		}
	}
	icns := filepath.Join(work, "AppIcon.icns")
	if output, err := exec.Command("/usr/bin/iconutil", "-c", "icns", iconset, "-o", icns).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build the app icon: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return os.ReadFile(icns)
}

// bundleStoreID reads DynAppStoreID from a bundle this agent generated.
func bundleStoreID(bundle string) string {
	data, err := os.ReadFile(filepath.Join(bundle, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`<key>DynAppStoreID</key>\s*<string>([^<]*)</string>`).FindSubmatch(data)
	if match == nil {
		return ""
	}
	return strings.ReplaceAll(string(match[1]), "&amp;", "&")
}

func nativeInstall(spec nativeInstallSpec) (nativeInstallResult, error) {
	root, err := nativeInstallRoot()
	if err != nil {
		return nativeInstallResult{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nativeInstallResult{}, err
	}
	icns, err := nativeIcns(spec.Icon)
	if err != nil {
		return nativeInstallResult{}, err
	}
	bundleID := nativeBundleID(spec.StoreID)
	name := nativeBundleName(spec.Name)
	target := filepath.Join(root, name+".app")
	if existing := bundleStoreID(target); existing != "" && existing != spec.StoreID {
		owner, _, _ := strings.Cut(spec.StoreID, "/")
		target = filepath.Join(root, name+" ("+nativeBundleName(owner)+").app")
	}
	files := map[string][]byte{
		"Contents/Info.plist":                    nativeInfoPlist(spec, bundleID),
		"Contents/PkgInfo":                       []byte("APPL????"),
		"Contents/MacOS/" + nativeHostExecutable: nativeHostBinary,
		"Contents/Resources/AppIcon.icns":        icns,
	}
	if !bundleMatches(target, files) {
		staging := filepath.Join(root, fmt.Sprintf(".%s.partial-%d", filepath.Base(target), os.Getpid()))
		_ = os.RemoveAll(staging)
		for relative, data := range files {
			path := filepath.Join(staging, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				_ = os.RemoveAll(staging)
				return nativeInstallResult{}, err
			}
			mode := os.FileMode(0o644)
			if strings.HasPrefix(relative, "Contents/MacOS/") {
				mode = 0o755
			}
			if err := os.WriteFile(path, data, mode); err != nil {
				_ = os.RemoveAll(staging)
				return nativeInstallResult{}, err
			}
		}
		if output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", "--identifier", bundleID, staging).CombinedOutput(); err != nil {
			_ = os.RemoveAll(staging)
			return nativeInstallResult{}, fmt.Errorf("sign the app: %w: %s", err, strings.TrimSpace(string(output)))
		}
		if err := os.RemoveAll(target); err != nil {
			_ = os.RemoveAll(staging)
			return nativeInstallResult{}, err
		}
		if err := os.Rename(staging, target); err != nil {
			_ = os.RemoveAll(staging)
			return nativeInstallResult{}, err
		}
	}
	// A renamed app leaves its previous bundle behind; remove it.
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			candidate := filepath.Join(root, entry.Name())
			if candidate != target && strings.HasSuffix(entry.Name(), ".app") && bundleStoreID(candidate) == spec.StoreID {
				lsregister("-u", candidate)
				_ = os.RemoveAll(candidate)
			}
		}
	}
	lsregister("-f", target)
	cdhash, err := bundleCDHash(target)
	if err != nil {
		return nativeInstallResult{}, err
	}
	return nativeInstallResult{
		Path: target, Executable: filepath.Join(target, "Contents", "MacOS", nativeHostExecutable),
		CDHash: cdhash, BundleID: bundleID,
	}, nil
}

// bundleMatches reports whether an existing signed bundle already has exactly
// these files, so reinstalling an unchanged app keeps its signature.
func bundleMatches(bundle string, files map[string][]byte) bool {
	for relative, data := range files {
		existing, err := os.ReadFile(filepath.Join(bundle, filepath.FromSlash(relative)))
		if err != nil || !bytes.Equal(existing, data) {
			return false
		}
	}
	if _, err := bundleCDHash(bundle); err != nil {
		return false
	}
	return exec.Command("/usr/bin/codesign", "--verify", "--strict", bundle).Run() == nil
}

func nativeUninstall(app NativeApp) error {
	root, err := nativeInstallRoot()
	if err != nil {
		return err
	}
	path := filepath.Clean(app.Path)
	if filepath.Dir(path) != root || !strings.HasSuffix(path, ".app") {
		return errors.New("the installed app is outside the DynApp apps folder")
	}
	if storeID := bundleStoreID(path); storeID != "" && storeID != app.StoreID {
		return errors.New("the app bundle belongs to a different app")
	}
	lsregister("-u", path)
	return os.RemoveAll(path)
}

func nativeLaunch(app NativeApp, _ string) error {
	if _, err := os.Stat(app.Path); err != nil {
		return errors.New("the app bundle is missing; install it again from Dyner")
	}
	output, err := exec.Command("/usr/bin/open", app.Path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open %s: %w: %s", app.Name, err, strings.TrimSpace(string(output)))
	}
	return nil
}
