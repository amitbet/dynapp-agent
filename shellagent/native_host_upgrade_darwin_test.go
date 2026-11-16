//go:build darwin

package shellagent

import (
	"bytes"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// installOldHostBundle installs a real bundle, then swaps in a different
// signed host to stand in for one an older agent wrote.
func installOldHostBundle(t *testing.T, oldHost []byte) (*Server, NativeApp) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and signs app bundles")
	}
	if supported, reason := nativeSupported(); !supported {
		t.Skip(reason)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	lsregisterEnabled = false
	t.Cleanup(func() { lsregisterEnabled = true })
	spec := nativeInstallSpec{StoreID: "amit-bet/notes", Name: "Notes", Origin: nativeNotesOrigin, URL: nativeNotesOrigin + "/", Icon: testPNG(t, color.RGBA{40, 120, 200, 255}), AgentEndpoint: "/tmp/agent.sock"}
	installed, err := nativeInstall(spec)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.WriteFile(installed.Executable, oldHost, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", "--identifier", installed.BundleID, installed.Path).CombinedOutput(); err != nil {
		t.Fatalf("re-sign: %v %s", err, output)
	}
	cdhash, err := bundleCDHash(installed.Path)
	if err != nil {
		t.Fatal(err)
	}
	app := NativeApp{StoreID: spec.StoreID, Name: spec.Name, Origin: spec.Origin, URL: spec.URL,
		Path: installed.Path, Executable: installed.Executable, CDHash: cdhash, BundleID: installed.BundleID,
		Capabilities: []string{"fs.read"}}
	return &Server{Config: Config{NativeApps: []NativeApp{app}}}, app
}

func TestNativeHostUpgradeRewritesOutdatedBundles(t *testing.T) {
	sleep, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skip(err)
	}
	server, app := installOldHostBundle(t, sleep)
	plist := mustRead(t, filepath.Join(app.Path, "Contents", "Info.plist"))
	icon := mustRead(t, filepath.Join(app.Path, "Contents", "Resources", "AppIcon.icns"))
	if !nativeHostOutdated(app) {
		t.Fatal("a bundle with another host must be outdated")
	}
	if waiting := server.upgradeNativeHosts(); waiting != 0 {
		t.Fatalf("waiting = %d", waiting)
	}
	upgraded, ok := server.nativeApp(app.StoreID)
	if !ok || upgraded.CDHash == app.CDHash || len(upgraded.CDHash) != 40 {
		t.Fatalf("record after upgrade = %#v", upgraded)
	}
	installed, _ := unsignedSum(mustRead(t, upgraded.Executable))
	if embedded, _ := embeddedHostSum(); installed != embedded || nativeHostOutdated(upgraded) {
		t.Fatal("the bundle does not run the embedded host")
	}
	if cdhash, err := bundleCDHash(upgraded.Path); err != nil || cdhash != upgraded.CDHash {
		t.Fatalf("recorded cdhash %s, bundle %s %v", upgraded.CDHash, cdhash, err)
	}
	if err := exec.Command("/usr/bin/codesign", "--verify", "--strict", upgraded.Path).Run(); err != nil {
		t.Fatalf("upgraded bundle does not verify: %v", err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(upgraded.Path, "Contents", "Info.plist")), plist) ||
		!bytes.Equal(mustRead(t, filepath.Join(upgraded.Path, "Contents", "Resources", "AppIcon.icns")), icon) {
		t.Fatal("the upgrade changed files other than the host")
	}
	if len(upgraded.Capabilities) != 1 || upgraded.Capabilities[0] != "fs.read" {
		t.Fatalf("the upgrade dropped the app's grant: %v", upgraded.Capabilities)
	}
	entries, _ := os.ReadDir(filepath.Dir(upgraded.Path))
	if len(entries) != 1 {
		t.Fatalf("staging or backup bundles left behind: %v", entries)
	}
	again, rewritten := server.upgradeNativeHost(upgraded)
	if rewritten || again.CDHash != upgraded.CDHash {
		t.Fatal("a current bundle was rewritten")
	}
}

func TestNativeHostUpgradeWaitsForRunningApp(t *testing.T) {
	// /bin/sleep stands in for an old host binary that is running.
	sleep, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skip(err)
	}
	server, app := installOldHostBundle(t, sleep)
	process := exec.Command(app.Executable, "30")
	if err := process.Start(); err != nil {
		t.Fatalf("start stand-in host: %v", err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for !nativeAppRunning(app) {
		if time.Now().After(deadline) {
			t.Fatal("the running host was not detected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if waiting := server.upgradeNativeHosts(); waiting != 1 {
		t.Fatalf("waiting = %d, want 1", waiting)
	}
	if current, _ := server.nativeApp(app.StoreID); current.CDHash != app.CDHash || !nativeHostOutdated(current) {
		t.Fatal("a running app's bundle was rewritten")
	}
	_ = process.Process.Kill()
	_ = process.Wait()
	if waiting := server.upgradeNativeHosts(); waiting != 0 {
		t.Fatalf("waiting after quit = %d", waiting)
	}
	if current, _ := server.nativeApp(app.StoreID); nativeHostOutdated(current) {
		t.Fatal("the bundle was not upgraded after the app quit")
	}
}
