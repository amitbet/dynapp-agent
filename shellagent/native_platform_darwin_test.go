//go:build darwin

package shellagent

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPNG(t *testing.T, fill color.Color) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			picture.Set(x, y, fill)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestNativeBundleIdentifiersAndNames(t *testing.T) {
	cases := map[string]string{
		"amit-bet/gridbook":   "io.dynapp.app.amit-bet.gridbook",
		"Some_Owner/My.App":   "io.dynapp.app.some-owner.my.app",
		"owner/slug_with__x_": "io.dynapp.app.owner.slug-with--x",
	}
	for storeID, want := range cases {
		if got := nativeBundleID(storeID); got != want {
			t.Fatalf("nativeBundleID(%q) = %q, want %q", storeID, got, want)
		}
	}
	if got := nativeBundleName("../Evil:Name\x01"); strings.ContainsAny(got, "/:\x01") || strings.HasPrefix(got, ".") {
		t.Fatalf("unsafe bundle name %q", got)
	}
}

func TestNativeInfoPlistIsDeterministicAndEscaped(t *testing.T) {
	spec := nativeInstallSpec{StoreID: "amit-bet/notes", Name: `Notes & <Co> "x"`, Origin: nativeNotesOrigin, URL: nativeNotesOrigin + "/?a=1&b=2", AgentEndpoint: "/tmp/agent.sock"}
	first := nativeInfoPlist(spec, nativeBundleID(spec.StoreID))
	if !bytes.Equal(first, nativeInfoPlist(spec, nativeBundleID(spec.StoreID))) {
		t.Fatal("Info.plist is not deterministic")
	}
	text := string(first)
	for _, want := range []string{"Notes &amp; &lt;Co&gt; &quot;x&quot;", "?a=1&amp;b=2", "<key>DynAppStoreID</key>\n\t<string>amit-bet/notes</string>", "<key>CFBundleVersion</key>\n\t<string>1</string>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Info.plist is missing %q:\n%s", want, text)
		}
	}
}

// TestNativeInstallKeepsAnUnchangedBundle builds real bundles (codesign,
// sips, iconutil) under a temporary home.
func TestNativeInstallKeepsAnUnchangedBundle(t *testing.T) {
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
	first, err := nativeInstall(spec)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if first.Path != filepath.Join(home, "Applications", "DynApp", "Notes.app") || first.BundleID != "io.dynapp.app.amit-bet.notes" || len(first.CDHash) != 40 {
		t.Fatalf("install result = %#v", first)
	}
	if info, err := os.Stat(first.Executable); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("host executable = %v %v", info, err)
	}
	again, err := nativeInstall(spec)
	if err != nil || again.CDHash != first.CDHash {
		t.Fatalf("unchanged reinstall changed the signature: %#v %v", again, err)
	}
	spec.Icon = testPNG(t, color.RGBA{200, 40, 40, 255})
	changed, err := nativeInstall(spec)
	if err != nil || changed.CDHash == first.CDHash {
		t.Fatalf("a new icon must produce a new signature: %#v %v", changed, err)
	}
	spec.Name = "Notes Pro"
	renamed, err := nativeInstall(spec)
	if err != nil || filepath.Base(renamed.Path) != "Notes Pro.app" {
		t.Fatalf("rename = %#v %v", renamed, err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("the old bundle was not removed after a rename: %v", err)
	}
	other := spec
	other.StoreID = "someone/notes"
	clash, err := nativeInstall(other)
	if err != nil || filepath.Base(clash.Path) != "Notes Pro (someone).app" {
		t.Fatalf("name clash = %#v %v", clash, err)
	}
	if err := nativeUninstall(NativeApp{StoreID: spec.StoreID, Path: renamed.Path}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if err := nativeUninstall(NativeApp{StoreID: spec.StoreID, Path: clash.Path}); err == nil {
		t.Fatal("uninstall removed another app's bundle")
	}
}
