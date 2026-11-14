package shellagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func testNote(name string) appNote {
	return appNote{StoreID: "amit-bet/formfuel", Name: name, Origin: "https://amitbet-formfuel.dynapp.io", Kind: appNoteKindNative}
}

func TestAppAgentNoteNamesTheAppAndPointsAtTheSkill(t *testing.T) {
	note := string(appAgentNote(testNote("Form & Fuel")))
	for _, want := range []string{
		"# Form & Fuel (DynApp)",
		"- App: `amit-bet/formfuel`",
		"- Address: https://amitbet-formfuel.dynapp.io/",
		"https://dynapp.io/api/v1/apps/amit-bet/formfuel",
		"dynapp-developer skill",
		"https://dynapp.io/skill/SKILL.md",
		"as a desktop app",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("note lacks %q:\n%s", want, note)
		}
	}
	if string(appAgentNote(testNote("Form & Fuel"))) != note {
		t.Fatal("note is not deterministic")
	}
	browser := testNote("Form & Fuel")
	browser.Kind = appNoteKindBrowser
	browser.DynerURL = "https://dyner.example/"
	text := string(appAgentNote(browser))
	if !strings.Contains(text, "web app (PWA)") || !strings.Contains(text, "https://dyner.example/skill/SKILL.md") {
		t.Fatalf("browser note:\n%s", text)
	}
	if noteStoreID([]byte(text)) != "amit-bet/formfuel" {
		t.Fatal("store id line does not parse back")
	}
}

func TestAppNoteFileNameIsSafeOnEveryOS(t *testing.T) {
	cases := map[string]string{
		"Form & Fuel":       "Form & Fuel",
		`a/b\c:d*e?"f<g>|h`: "a b c d e f g h",
		"  ..hidden. ":      "hidden",
		"":                  "DynApp",
		"\x01":              "DynApp",
	}
	for input, want := range cases {
		if got := appNoteFileName(input); got != want {
			t.Errorf("appNoteFileName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWriteAppNoteReplacesRenamedNoteAndKeepsOtherApps(t *testing.T) {
	dir := t.TempDir()
	if err := writeAppNote(dir, testNote("Form and Fuel")); err != nil {
		t.Fatal(err)
	}
	other := appNote{StoreID: "maya42/formfuel", Name: "Form & Fuel", Origin: "https://maya42-formfuel.dynapp.io", Kind: appNoteKindBrowser}
	if err := writeAppNote(dir, other); err != nil {
		t.Fatal(err)
	}
	// The renamed app takes the same display name as maya42's copy.
	if err := writeAppNote(dir, testNote("Form & Fuel")); err != nil {
		t.Fatal(err)
	}
	names := listDir(t, dir)
	want := []string{"AGENTS.md", "Form & Fuel (amit-bet).dynapp.md", "Form & Fuel.dynapp.md"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("files = %v, want %v", names, want)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "Form & Fuel.dynapp.md")); noteStoreID(data) != "maya42/formfuel" {
		t.Fatal("another app's note was overwritten")
	}
	readme, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(readme), "https://dynapp.io/skill/SKILL.md") {
		t.Fatalf("folder readme:\n%s", readme)
	}

	removeAppNote(dir, "amit-bet/formfuel")
	if names := listDir(t, dir); strings.Join(names, "|") != "AGENTS.md|Form & Fuel.dynapp.md" {
		t.Fatalf("after remove: %v", names)
	}
}

func TestWriteAppNoteRejectsInvalidStoreID(t *testing.T) {
	if err := writeAppNote(t.TempDir(), appNote{StoreID: "../x", Name: "X"}); err == nil {
		t.Fatal("invalid store id accepted")
	}
}

func TestNoteBrowserInstallWritesOnlyVerifiedHostedInstalls(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "android" {
		t.Skip("the notes folder resolves the signed-in user's profile")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	dir, err := appNotesDir()
	if err != nil || !strings.HasPrefix(dir, home) {
		t.Fatalf("appNotesDir() = %q, %v", dir, err)
	}
	server := &Server{}
	auth := socketAuthentication{storeID: "amit-bet/gridbook", appName: "Gridbook", origin: "https://amitbet-gridbook.dynapp.io", ok: true}

	server.noteBrowserInstall(socketAuthentication{storeID: "amit-bet/dev", appName: "Dev", origin: "http://localhost:5173", development: true})
	server.noteBrowserInstall(socketAuthentication{storeID: "amit-bet/plain", appName: "Plain", origin: "http://example.test"})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("development or non-HTTPS app wrote a note: %v", listDir(t, dir))
	}

	server.noteBrowserInstall(auth)
	data, err := os.ReadFile(filepath.Join(dir, "Gridbook.dynapp.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "web app (PWA)") || noteStoreID(data) != "amit-bet/gridbook" {
		t.Fatalf("note:\n%s", data)
	}

	// A native install of the same app keeps its own note.
	removeAppNote(dir, "amit-bet/gridbook")
	server.Config.NativeApps = []NativeApp{{StoreID: "amit-bet/gridbook"}}
	server.noteBrowserInstall(auth)
	if _, err := os.Stat(filepath.Join(dir, "Gridbook.dynapp.md")); !os.IsNotExist(err) {
		t.Fatal("browser note written over a native install")
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestInstalledPWAHandshakeWritesBrowserNote(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "android" {
		t.Skip("the notes folder resolves the signed-in user's profile")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	dyner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/amit-bet/notes" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"app":            map[string]any{"id": "amit-bet/notes", "name": "Notes"},
			"hostedOrigins":  []string{"https://amitbet-notes.dynapp.io"},
			"latestRevision": map[string]any{"id": "rev_1", "manifest": map[string]any{"name": "Notes", "backendPermissions": []any{"fs.readText"}}},
		})
	}))
	defer dyner.Close()
	agent := &Server{StateDir: t.TempDir(), autoApprovePairings: true, Config: Config{DynerBaseURL: dyner.URL, AppDomain: "dynapp.io"}, DynerHTTPClient: dyner.Client()}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir, _ := appNotesDir()
	note := filepath.Join(dir, "Notes.dynapp.md")

	tab, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "tab"}})
	tab.Close(websocket.StatusNormalClosure, "")
	phone, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "pwa", Mobile: true}})
	phone.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(note); !os.IsNotExist(err) {
		t.Fatal("a browser tab or a mobile client wrote a note")
	}

	installed, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "pwa"}})
	defer installed.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(note)
		if err == nil {
			if noteStoreID(data) != "amit-bet/notes" || !strings.Contains(string(data), dyner.URL+"/skill/SKILL.md") {
				t.Fatalf("note:\n%s", data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no note at %s: %v", note, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
