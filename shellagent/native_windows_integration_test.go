//go:build windows

package shellagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestNativeWindowsExtensionsAndIdentity(t *testing.T) {
	got := nativeWindowsExtensionsJSON([]nativeDocumentType{{Extensions: []string{".TXT", "txt", "md", "../bad", "*", "x\\y", ""}}})
	if got != `[".txt",".md"]` {
		t.Fatalf("extensions = %s", got)
	}
	if nativeWindowsRegistrationID("a/notes") == nativeWindowsRegistrationID("b/notes") {
		t.Fatal("owners share an identity")
	}
}

// Execute the real PowerShell/COM installer against a private registry subtree
// and temporary desktop, without changing the user's registrations or shortcuts.
func TestNativeWindowsIntegrationLifecycle(t *testing.T) {
	dir := t.TempDir()
	desktop := filepath.Join(dir, "Desktop")
	if err := os.MkdirAll(desktop, 0700); err != nil {
		t.Fatal(err)
	}
	base := `Software\DynAppIntegrationTest` + time.Now().Format("20060102150405.000000000")
	defer registry.DeleteKey(registry.CURRENT_USER, base)
	t.Cleanup(func() {
		script := `$ErrorActionPreference='Stop'; [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree('` + base + `', $false)`
		if output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedPowerShell(script)).CombinedOutput(); err != nil {
			t.Errorf("cleanup: %v %s", err, output)
		}
	})
	script := strings.Replace(nativeWindowsIntegrationScript, "$base = 'Software'", "$base = '"+base+"'", 1)
	script = strings.Replace(script, "[Environment]::GetFolderPath('DesktopDirectory')", "'"+strings.ReplaceAll(desktop, "'", "''")+"'", 1)
	id := nativeWindowsRegistrationID("owner/notes")
	other := nativeWindowsRegistrationID("other/notes")
	run := func(mode, appID, name, extensions string) {
		t.Helper()
		link := filepath.Join(dir, name+".lnk")
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedPowerShell(shortcutScript+script))
		cmd.Env = append(os.Environ(), "DYNAPP_NATIVE_MODE="+mode, "DYNAPP_NATIVE_ID="+appID, "DYNAPP_NATIVE_NAME="+name, "DYNAPP_NATIVE_EXTENSIONS="+extensions,
			"DYNAPP_LNK_PATH="+link, "DYNAPP_LNK_TARGET="+filepath.Join(dir, "agent.exe"), "DYNAPP_LNK_ARGS="+joinWindowsArgs(nativeHostArgs(NativeApp{StoreID: "owner/notes", Name: name, Origin: "https://example.com", URL: "https://example.com/"}, `\\.\pipe\test`, filepath.Join(dir, "icon.ico"))), "DYNAPP_LNK_ICON="+filepath.Join(dir, "icon.ico"), "DYNAPP_LNK_APPID=DynApp.owner.notes", "DYNAPP_LNK_DESCRIPTION="+name)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, output)
		}
	}
	get := func(path, name string) string {
		t.Helper()
		key, err := registry.OpenKey(registry.CURRENT_USER, base+`\`+path, registry.QUERY_VALUE)
		if err != nil {
			t.Fatal(err)
		}
		defer key.Close()
		value, _, err := key.GetStringValue(name)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	has := func(path, name string) bool {
		key, err := registry.OpenKey(registry.CURRENT_USER, base+`\`+path, registry.QUERY_VALUE)
		if err != nil {
			return false
		}
		defer key.Close()
		names, err := key.ReadValueNames(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range names {
			if value == name {
				return true
			}
		}
		return false
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\Classes\.txt`, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	_ = key.SetStringValue("", "Existing.Editor")
	key.Close()
	run("install", id, "Notes", `[".txt",".md"]`)
	if _, err := os.Stat(filepath.Join(desktop, "Notes.lnk")); err != nil {
		t.Fatal(err)
	}
	command := get(`Classes\`+id+`\shell\open\command`, "")
	if !strings.Contains(command, ` app --id owner/notes `) || !strings.HasSuffix(command, ` -- "%1"`) {
		t.Fatalf("command = %s", command)
	}
	if get(`Classes\.txt`, "") != "Existing.Editor" {
		t.Fatal("overwrote the existing default")
	}
	if !has(`Classes\.txt\OpenWithProgids`, id) || get(`RegisteredApplications`, id) == "" {
		t.Fatalf("missing registration: openWith=%v registered=%q id=%s", has(`Classes\.txt\OpenWithProgids`, id), get(`RegisteredApplications`, id), id)
	}
	run("install", other, "Other Notes", `[".txt"]`)
	run("install", id, "Renamed Notes", `[".md"]`)
	if has(`Classes\.txt\OpenWithProgids`, id) || !has(`Classes\.txt\OpenWithProgids`, other) {
		t.Fatal("reinstall failed to preserve other apps")
	}
	if _, err := os.Stat(filepath.Join(desktop, "Notes.lnk")); !os.IsNotExist(err) {
		t.Fatal("old desktop shortcut remains")
	}
	run("uninstall", id, "Renamed Notes", `[]`)
	if has(`RegisteredApplications`, id) || has(`Classes\.md\OpenWithProgids`, id) {
		t.Fatal("uninstall left registrations")
	}
	if _, err := os.Stat(filepath.Join(desktop, "Renamed Notes.lnk")); !os.IsNotExist(err) {
		t.Fatal("desktop shortcut remains")
	}
	if !has(`Classes\.txt\OpenWithProgids`, other) || get(`Classes\.txt`, "") != "Existing.Editor" {
		t.Fatal("uninstall changed another app")
	}
	run("install", id, "No Types", `[]`)
	if has(`RegisteredApplications`, id) {
		t.Fatal("app without file types claims formats")
	}
}
