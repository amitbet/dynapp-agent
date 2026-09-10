//go:build darwin

package assoc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledChromePWAFindsHostedAppIgnoringLaunchQuery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plist := filepath.Join(home, "Applications", "Chrome Apps.localized", "Markdown Viewer.app", "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.google.Chrome.app.mdview</string><key>CrAppModeShortcutURL</key><string>https://amitbet-mdview.dynapp.io/?launch=pwa</string></dict></plist>`)
	if err := os.WriteFile(plist, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bundleID := installedChromePWA("https://amitbet-mdview.dynapp.io/")
	if bundleID != "com.google.Chrome.app.mdview" {
		t.Fatalf("installed Chrome PWA = %q", bundleID)
	}
}

func TestInstalledChromePWACanMatchDisplayNameAcrossURLForms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plist := filepath.Join(home, "Applications", "Markdown Viewer.app", "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.google.Chrome.app.mdview</string><key>CFBundleDisplayName</key><string>Markdown_Viewer</string><key>CrAppModeShortcutURL</key><string>https://old.example.test/markdown</string></dict></plist>`)
	if err := os.WriteFile(plist, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if bundleID := installedChromePWA("https://amitbet-mdview.dynapp.io/", "Markdown Viewer"); bundleID != "com.google.Chrome.app.mdview" {
		t.Fatalf("installed Chrome PWA by name = %q", bundleID)
	}
}

func TestSameHostedAppURL(t *testing.T) {
	if !sameHostedAppURL("https://example.test/app/?launch=pwa", "https://example.test/app") {
		t.Fatal("launch query should not change the hosted app identity")
	}
	if !sameHostedAppURL("https://dynapp.io/app/amit-bet/mdview?launch=pwa", "https://amitbet-mdview.dynapp.io/") {
		t.Fatal("legacy and app-host URLs should identify the same hosted app")
	}
	if sameHostedAppURL("https://example.test/other", "https://example.test/app") {
		t.Fatal("different paths must not match")
	}
}
