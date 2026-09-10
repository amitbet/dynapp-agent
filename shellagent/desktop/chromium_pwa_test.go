package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchChromiumPWADesktopInsertsNoSandbox(t *testing.T) {
	original := "[Desktop Entry]\nType=Application\nName=Dyner\nExec=/usr/bin/chromium --profile-directory=Default --app-id=eanieodajmecedlgmphpcmdahgkdpimk\n"
	updated, changed := patchChromiumPWADesktop(original)
	if !changed {
		t.Fatal("expected Exec to gain --no-sandbox")
	}
	if !strings.Contains(updated, "Exec=/usr/bin/chromium --no-sandbox --profile-directory=Default --app-id=eanieodajmecedlgmphpcmdahgkdpimk") {
		t.Fatalf("updated Exec = %q", updated)
	}
	again, changed := patchChromiumPWADesktop(updated)
	if changed {
		t.Fatalf("second patch changed file: %q", again)
	}
}

func TestPatchChromiumPWADesktopPreservesShebang(t *testing.T) {
	original := "#!/usr/bin/env xdg-open\n[Desktop Entry]\nExec=/usr/bin/chromium --profile-directory=Default --app-id=abc\n"
	updated, changed := patchChromiumPWADesktop(original)
	if !changed || !strings.HasPrefix(updated, "#!/usr/bin/env xdg-open\n") {
		t.Fatalf("shebang lost: %q", updated)
	}
	if !strings.Contains(updated, "Exec=/usr/bin/chromium --no-sandbox --profile-directory=Default --app-id=abc") {
		t.Fatalf("updated Exec = %q", updated)
	}
}

func TestPatchChromiumPWADesktopIgnoresNonPWAExec(t *testing.T) {
	original := "[Desktop Entry]\nExec=/usr/bin/chromium https://dynapp.io\n"
	if _, changed := patchChromiumPWADesktop(original); changed {
		t.Fatal("browser Exec without --app-id should be left alone")
	}
}

func TestChromiumPWARepairWritesDesktopAndApplications(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	applications := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(applications, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Desktop Entry]\nType=Application\nName=Dyner\nExec=/usr/bin/chromium --profile-directory=Default --app-id=eanieodajmecedlgmphpcmdahgkdpimk\n"
	desktopFile := filepath.Join(desktop, "chrome-eanieodajmecedlgmphpcmdahgkdpimk-Default.desktop")
	appFile := filepath.Join(applications, "chrome-eanieodajmecedlgmphpcmdahgkdpimk-Default.desktop")
	if err := os.WriteFile(desktopFile, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(desktop, "notes.txt")
	if err := os.WriteFile(other, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	repair := chromiumPWARepair{home: home}
	if patched := repair.once(); patched != 2 {
		t.Fatalf("patched %d files", patched)
	}
	for _, path := range []string{desktopFile, appFile} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "--no-sandbox") {
			t.Fatalf("%s was not repaired: %s", path, data)
		}
	}
	unchanged, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unchanged), "--no-sandbox") {
		t.Fatal("non-desktop file was patched")
	}
}
