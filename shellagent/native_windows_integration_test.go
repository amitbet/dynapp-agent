//go:build windows

package shellagent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestNativeWindowsExtensionsAndIdentity(t *testing.T) {
	got := nativeWindowsExtensions([]nativeDocumentType{{Extensions: []string{".TXT", "txt", "md", "../bad", "*", "x\\y", ""}}})
	if strings.Join(got, ",") != ".txt,.md" {
		t.Fatalf("extensions = %v", got)
	}
	if nativeWindowsRegistrationID("a/notes") == nativeWindowsRegistrationID("b/notes") {
		t.Fatal("owners share an identity")
	}
}

// Run the real COM/registry integration against a private registry subtree
// and temporary desktop, without changing the user's registrations or shortcuts.
func TestNativeWindowsIntegrationLifecycle(t *testing.T) {
	dir := t.TempDir()
	desktop := filepath.Join(dir, "Desktop")
	if err := os.MkdirAll(desktop, 0700); err != nil {
		t.Fatal(err)
	}
	base := `Software\DynAppIntegrationTest` + time.Now().Format("20060102150405.000000000")
	t.Cleanup(func() {
		if err := deleteRegistryTree(registry.CURRENT_USER, base); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	id := nativeWindowsRegistrationID("owner/notes")
	other := nativeWindowsRegistrationID("other/notes")
	run := func(mode, appID, name string, extensions []string) {
		t.Helper()
		integration := nativeWindowsIntegration{
			Root: registry.CURRENT_USER, Base: base, Desktop: desktop, Uninstall: mode == "uninstall",
			ID: appID, Name: name, Extensions: extensions,
			Shortcut: windowsShortcut{
				Path: filepath.Join(dir, name+".lnk"), Target: filepath.Join(dir, "agent.exe"),
				Args: joinWindowsArgs(nativeHostArgs(NativeApp{StoreID: "owner/notes", Name: name, Origin: "https://example.com", URL: "https://example.com/"}, `\\.\pipe\test`, filepath.Join(dir, "icon.ico"))),
				Icon: filepath.Join(dir, "icon.ico"), AppID: "DynApp.owner.notes", Description: name,
			},
		}
		if err := integration.apply(); err != nil {
			t.Fatalf("%s: %v", mode, err)
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
	run("install", id, "Notes", []string{".txt", ".md"})
	link, err := os.ReadFile(filepath.Join(desktop, "Notes.lnk"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(link, utf16Bytes("DynApp.owner.notes")) {
		t.Fatal("shortcut has no AppUserModelID")
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
	run("install", other, "Other Notes", []string{".txt"})
	run("install", id, "Renamed Notes", []string{".md"})
	if has(`Classes\.txt\OpenWithProgids`, id) || !has(`Classes\.txt\OpenWithProgids`, other) {
		t.Fatal("reinstall failed to preserve other apps")
	}
	if _, err := os.Stat(filepath.Join(desktop, "Notes.lnk")); !os.IsNotExist(err) {
		t.Fatal("old desktop shortcut remains")
	}
	run("uninstall", id, "Renamed Notes", nil)
	if has(`RegisteredApplications`, id) || has(`Classes\.md\OpenWithProgids`, id) {
		t.Fatal("uninstall left registrations")
	}
	if _, err := os.Stat(filepath.Join(desktop, "Renamed Notes.lnk")); !os.IsNotExist(err) {
		t.Fatal("desktop shortcut remains")
	}
	if !has(`Classes\.txt\OpenWithProgids`, other) || get(`Classes\.txt`, "") != "Existing.Editor" {
		t.Fatal("uninstall changed another app")
	}
	run("install", id, "No Types", nil)
	if has(`RegisteredApplications`, id) {
		t.Fatal("app without file types claims formats")
	}
}
