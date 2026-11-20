//go:build linux && !android

package shellagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linuxTestPNG(t *testing.T, fill color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, fill)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestNativeLinuxAppID(t *testing.T) {
	for storeID, want := range map[string]string{
		"amit-bet/gridbook": "io.dynapp.app.amit_bet.gridbook",
		"Owner/2048":        "io.dynapp.app.owner._2048",
		"a.b/c d":           "io.dynapp.app.a_b.c_d",
		"x/--":              "io.dynapp.app.x.app",
	} {
		if got := nativeLinuxAppID(storeID); got != want {
			t.Errorf("nativeLinuxAppID(%q) = %q, want %q", storeID, got, want)
		}
	}
}

func TestDesktopExecArgQuoting(t *testing.T) {
	for value, want := range map[string]string{
		"/usr/bin/dynapp-app-host": "/usr/bin/dynapp-app-host",
		"Grid Book":                `"Grid Book"`,
		`say "hi"`:                 `"say \\"hi\\""`,
		"100%":                     "100%%",
		"$HOME":                    `"\\$HOME"`,
		`back\slash`:               `"back\\\\slash"`,
	} {
		if got := desktopExecArg(value); got != want {
			t.Errorf("desktopExecArg(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestNativeLinuxMimeTypes(t *testing.T) {
	types, xml := nativeLinuxMimeTypes("amit-bet/gridbook", "Gridbook", []nativeDocumentType{
		{Name: "Spreadsheet", Extensions: []string{"xlsx"}, MimeTypes: []string{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}},
		{Name: "Grid", Extensions: []string{".grid", "bad ext", "grid"}},
	})
	want := []string{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/x-dynapp-amit-bet-gridbook-grid"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("mime types = %v", types)
	}
	if !strings.Contains(string(xml), `<glob pattern="*.grid"/>`) || strings.Contains(string(xml), "bad ext") || strings.Contains(string(xml), "xlsx") {
		t.Fatalf("mime package = %s", xml)
	}
	if _, xml := nativeLinuxMimeTypes("a/b", "B", nil); xml != nil {
		t.Fatalf("no document types still wrote a package: %s", xml)
	}
}

func TestNativeLinuxInstallAndUninstall(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	host := filepath.Join(t.TempDir(), nativeHostExecutable)
	if err := os.WriteFile(host, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DYNAPP_APP_HOST", host)
	spec := nativeInstallSpec{
		StoreID: "amit-bet/grid-book", Name: "Grid \"Book\" 100%", Origin: "https://amitbet-grid-book.dynapp.io",
		URL: "https://amitbet-grid-book.dynapp.io/?launch=pwa", Icon: linuxTestPNG(t, color.RGBA{1, 2, 3, 255}),
		AgentEndpoint: "/run/user/1000/dynapp/native-host.sock",
		DocumentTypes: []nativeDocumentType{{Name: "Grid", Extensions: []string{"grid"}}},
	}
	result, err := nativeInstall(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Executable != host || result.BundleID != "io.dynapp.app.amit_bet.grid_book" {
		t.Fatalf("result = %+v", result)
	}
	entry, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(entry)
	for _, want := range []string{
		"Name=Grid \"Book\" 100%\n",
		"Exec=" + host + " --id amit-bet/grid-book --app-id io.dynapp.app.amit_bet.grid_book --socket /run/user/1000/dynapp/native-host.sock",
		` --name "Grid \\"Book\\" 100%%"`,
		" -- %F\n",
		"StartupWMClass=io.dynapp.app.amit_bet.grid_book\n",
		"MimeType=application/x-dynapp-amit-bet-grid-book-grid;\n",
		"X-DynApp-StoreID=amit-bet/grid-book\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("desktop entry lacks %q:\n%s", want, text)
		}
	}
	if filepath.Base(result.Path) != "io.dynapp.app.amit_bet.grid_book.desktop" {
		t.Fatalf("desktop file = %s", result.Path)
	}
	icon := filepath.Join(data, "dynapp", "native", "amit-bet", "grid-book", "icon.png")
	if _, err := os.Stat(icon); err != nil {
		t.Fatalf("icon: %v", err)
	}
	if !strings.Contains(text, "Icon="+icon+"\n") {
		t.Fatalf("desktop entry icon:\n%s", text)
	}
	mime := filepath.Join(data, "mime", "packages", "io.dynapp.app.amit_bet.grid_book.xml")
	if _, err := os.Stat(mime); err != nil {
		t.Fatalf("mime package: %v", err)
	}
	app := NativeApp{StoreID: spec.StoreID, Path: result.Path}
	if err := nativeUninstall(NativeApp{StoreID: "other/app", Path: result.Path}); err == nil {
		t.Fatal("uninstalled another app's entry")
	}
	if err := nativeUninstall(app); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.Path, mime, icon} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after uninstall", path)
		}
	}
}

func TestNativeLinuxSupportWithoutHost(t *testing.T) {
	t.Setenv("DYNAPP_APP_HOST", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("PATH", t.TempDir())
	nativeSupportCache.Lock()
	nativeSupportCache.at = nativeSupportCache.at.AddDate(-1, 0, 0)
	nativeSupportCache.Unlock()
	if supported, reason := checkNativeSupport(); supported || !strings.Contains(reason, "dynapp-app-host") {
		t.Fatalf("support without a host = %v %q", supported, reason)
	}
}

// fakeHostRelease serves release v1.2.3 with a signed host whose --version
// prints 1.2.3. *signature can be changed to break the signature.
func fakeHostRelease(t *testing.T) (*httptest.Server, *string, func([]byte) string) {
	sign := useTestReleaseKey(t)
	binary := []byte("#!/bin/sh\necho 1.2.3\n")
	sum := sha256.Sum256(binary)
	base := nativeHostAssetBase("1.2.3")
	signature := sign(binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		switch r.URL.Path {
		case "/repos/amitbet/dynapp-agent/releases/tags/v1.2.3":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3", "assets": []map[string]any{
				{"name": base, "browser_download_url": origin + "/bin", "size": len(binary)},
				{"name": base + ".sha256", "browser_download_url": origin + "/sum", "size": 64},
				{"name": base + ".sig", "browser_download_url": origin + "/sig", "size": len(signature)},
			}})
		case "/bin":
			_, _ = w.Write(binary)
		case "/sum":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), base)
		case "/sig":
			_, _ = w.Write([]byte(signature))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &signature, sign
}

func TestReplaceNativeHostBinary(t *testing.T) {
	server, signature, sign := fakeHostRelease(t)
	config := SelfUpdateConfig{Repository: "amitbet/dynapp-agent", APIBaseURL: server.URL, Version: "1.2.3"}
	host := filepath.Join(t.TempDir(), nativeHostExecutable)
	if err := os.WriteFile(host, []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := nativeHostVersion(ctx, host); got != "1.0.0" {
		t.Fatalf("installed version = %q", got)
	}
	if err := replaceNativeHostBinary(ctx, config, host, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if got := nativeHostVersion(ctx, host); got != "1.2.3" {
		t.Fatalf("updated version = %q", got)
	}
	// A bad signature leaves the installed host alone.
	*signature = sign([]byte("something else"))
	if err := os.WriteFile(host, []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceNativeHostBinary(ctx, config, host, "1.2.3"); err == nil {
		t.Fatal("a host with a bad signature was installed")
	}
	if got := nativeHostVersion(ctx, host); got != "1.0.0" {
		t.Fatalf("host after a refused update = %q", got)
	}
}

// A desktop without a host gets one from the agent; install.sh no longer
// downloads it.
func TestAgentDownloadsMissingNativeHost(t *testing.T) {
	if nativeHostPath() != "" && os.Getenv("DYNAPP_APP_HOST") == "" {
		t.Skip("a host is installed on this machine")
	}
	release, _, _ := fakeHostRelease(t)
	t.Setenv("DYNAPP_APP_HOST", "")
	t.Setenv("DYNAPP_NATIVE_APPS", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if nativeHostPath() != "" {
		t.Skip("a host is installed on this machine")
	}
	target := nativeHostInstallTarget()
	t.Cleanup(func() { _ = os.Remove(target) })
	server := &Server{selfUpdateConfig: SelfUpdateConfig{Enabled: true, Repository: "amitbet/dynapp-agent", APIBaseURL: release.URL, Version: "1.2.3"}}
	if err := server.maintainNativeHost(); err != nil {
		t.Fatal(err)
	}
	if got := nativeHostPath(); got != target {
		t.Fatalf("host = %q, want %q", got, target)
	}
	if got := nativeHostVersion(context.Background(), target); got != "1.2.3" {
		t.Fatalf("downloaded host version = %q", got)
	}
	// Turned off: nothing is fetched.
	_ = os.Remove(target)
	t.Setenv("DYNAPP_NATIVE_APPS", "0")
	if err := server.maintainNativeHost(); err != nil || nativeHostPath() != "" {
		t.Fatalf("fetched a host with native apps off: %v", err)
	}
}
