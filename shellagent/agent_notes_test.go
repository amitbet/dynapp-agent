package shellagent

import (
	"context"
	"encoding/json"
	"net"
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

func TestAppAgentNoteNamesTheAppAndPointsAtTheSkill(t *testing.T) {
	note := string(appAgentNote(appNote{StoreID: "amit-bet/formfuel", Name: "Form & Fuel", Origin: "https://amitbet-formfuel.dynapp.io", Kind: appNoteKindNative}))
	for _, want := range []string{
		"# Form & Fuel (DynApp)",
		"- App: `amit-bet/formfuel`",
		"- Address: https://amitbet-formfuel.dynapp.io/",
		"https://dynapp.io/api/v1/apps/amit-bet/formfuel",
		"dynapp-developer skill",
		"https://dynapp.io/skill/SKILL.md",
		"needs network access to dynapp.io",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("note lacks %q:\n%s", want, note)
		}
	}
}

func TestInstalledAppsMarkdownIsSortedAndEscaped(t *testing.T) {
	apps := []indexedApp{
		{StoreID: "amit-bet/portscope", Name: "Portscope", Origin: "https://amitbet-portscope.dynapp.io", Kind: appNoteKindBrowser},
		{StoreID: "amit-bet/formfuel", Name: "Form | Fuel", Origin: "https://amitbet-formfuel.dynapp.io", Kind: appNoteKindNative},
	}
	text := string(installedAppsMarkdown(apps, "", "/skills/SKILL.md"))
	first := strings.Index(text, "Form \\| Fuel")
	second := strings.Index(text, "| Portscope | `amit-bet/portscope` | https://amitbet-portscope.dynapp.io/ | web app (from the browser) |")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("index:\n%s", text)
	}
	if !strings.Contains(text, "a copy is at /skills/SKILL.md") {
		t.Fatalf("index lacks the local skill:\n%s", text)
	}
	if string(installedAppsMarkdown([]indexedApp{apps[1], apps[0]}, "", "/skills/SKILL.md")) != text {
		t.Fatal("index depends on input order")
	}
	if empty := string(installedAppsMarkdown(nil, "", "")); !strings.Contains(empty, "No apps are installed right now.") {
		t.Fatalf("empty index:\n%s", empty)
	}
}

// isolateCodingAgents points the agents folder, the skill download and the
// coding-agent homes at temporary places.
func isolateCodingAgents(t *testing.T) (*Server, string) {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "android" {
		t.Skip("resolves the signed-in user's profile")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CODEX_HOME", "")
	resetLocalSkillRefresh(t)
	dyner := skillServer(t, skillZip(t, map[string]string{"dynapp-developer/SKILL.md": "skill"}))
	dir, err := codingAgentsDir()
	if err != nil || !strings.HasPrefix(dir, home) {
		t.Fatalf("codingAgentsDir() = %q, %v", dir, err)
	}
	return &Server{DynerHTTPClient: dyner.Client(), Config: Config{DynerBaseURL: dyner.URL}}, dir
}

func TestNoteBrowserInstallListsOnlyVerifiedHostedInstalls(t *testing.T) {
	server, dir := isolateCodingAgents(t)
	index := filepath.Join(dir, installedAppsIndexName)

	server.noteBrowserInstall(socketAuthentication{storeID: "amit-bet/dev", appName: "Dev", origin: "http://localhost:5173", development: true})
	server.noteBrowserInstall(socketAuthentication{storeID: "amit-bet/plain", appName: "Plain", origin: "http://example.test"})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a development or non-HTTPS app created the agents folder")
	}

	auth := socketAuthentication{storeID: "amit-bet/gridbook", appName: "Gridbook", origin: "https://amitbet-gridbook.dynapp.io", ok: true}
	server.noteBrowserInstall(auth)
	text := string(mustRead(t, index))
	if !strings.Contains(text, "| Gridbook | `amit-bet/gridbook` | https://amitbet-gridbook.dynapp.io/ | web app (from the browser) |") {
		t.Fatalf("index:\n%s", text)
	}
	if localSkillPath() == "" || !strings.Contains(text, localSkillPath()) {
		t.Fatal("index does not name the local skill copy")
	}

	// A native install of the same app is listed once, as a desktop app.
	bundle := filepath.Join(t.TempDir(), "Gridbook.app")
	_ = os.MkdirAll(bundle, 0o755)
	server.Config.NativeApps = []NativeApp{{StoreID: "amit-bet/gridbook", Name: "Gridbook", Origin: "https://amitbet-gridbook.dynapp.io", Path: bundle}}
	server.refreshInstalledApps(context.Background())
	text = string(mustRead(t, index))
	if strings.Count(text, "`amit-bet/gridbook`") != 1 || !strings.Contains(text, "| desktop app |") {
		t.Fatalf("index with a native install:\n%s", text)
	}

	// Uninstalling everything leaves an index that says so.
	server.Config.NativeApps = nil
	_ = os.Remove(filepath.Join(dir, browserAppsFileName))
	server.refreshInstalledApps(context.Background())
	if text := string(mustRead(t, index)); !strings.Contains(text, "No apps are installed right now.") {
		t.Fatalf("index after uninstall:\n%s", text)
	}
}

func TestBrowserInstallsNotOpenedForMonthsAreForgotten(t *testing.T) {
	server, dir := isolateCodingAgents(t)
	_ = os.MkdirAll(dir, 0o755)
	old := time.Now().Add(-browserAppForgetAfter - time.Hour).UnixMilli()
	_ = writeBrowserApps(dir, []indexedApp{
		{StoreID: "amit-bet/old", Name: "Old", Origin: "https://amitbet-old.dynapp.io", Kind: appNoteKindBrowser, SeenAt: old},
		{StoreID: "amit-bet/new", Name: "New", Origin: "https://amitbet-new.dynapp.io", Kind: appNoteKindBrowser, SeenAt: time.Now().UnixMilli()},
	})
	server.refreshInstalledApps(context.Background())
	text := string(mustRead(t, filepath.Join(dir, installedAppsIndexName)))
	if strings.Contains(text, "amit-bet/old") || !strings.Contains(text, "amit-bet/new") {
		t.Fatalf("index:\n%s", text)
	}
	var kept []indexedApp
	_ = json.Unmarshal(mustRead(t, filepath.Join(dir, browserAppsFileName)), &kept)
	if len(kept) != 1 || kept[0].StoreID != "amit-bet/new" {
		t.Fatalf("browser apps = %#v", kept)
	}
}

func TestInstalledPWAHandshakeListsTheApp(t *testing.T) {
	_, dir := isolateCodingAgents(t)
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
	index := filepath.Join(dir, installedAppsIndexName)

	tab, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "tab"}})
	tab.Close(websocket.StatusNormalClosure, "")
	phone, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "pwa", Mobile: true}})
	phone.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatal("a browser tab or a mobile client listed the app")
	}

	installed, _ := dialTestAgent(t, ctx, server, testDialOptions{Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Client: &BrowserClient{Browser: "Chrome", Mode: "pwa"}})
	defer installed.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(index)
		if err == nil && strings.Contains(string(data), "`amit-bet/notes`") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("index = %q, %v", data, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoopbackPeerAcceptsOnlyThisComputer(t *testing.T) {
	cases := map[string]bool{"127.0.0.1:5000": true, "[::1]:5000": true, "192.168.1.20:5000": false, "100.64.0.1:9011": false, "[fe80::1]:5000": false}
	for address, want := range cases {
		addr, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			t.Fatal(err)
		}
		if got := loopbackPeer(addr); got != want {
			t.Errorf("loopbackPeer(%s) = %v, want %v", address, got, want)
		}
	}
	if loopbackPeer(nil) {
		t.Error("nil address treated as loopback")
	}
}
