package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A user tells their coding agent "change my Gridbook app". The agent reads one
// line in its global instructions (coding_agents.go) that points at
// installed-apps.md in the coding-agents folder. That index lists every app
// installed on this computer, with its store id and address, and says where
// the dynapp-developer skill is. The index holds facts only; the workflow
// lives in the skill (docs/agent-notes.md in the DynApp repository).
//
// A native install also carries a per-app AGENTS.md (inside the macOS bundle,
// in the Windows app folder), so a search for the app's name still lands on
// something useful for coding agents that never read the global line.

const (
	installedAppsIndexName = "installed-apps.md"
	browserAppsFileName    = "browser-apps.json"
	appNoteKindNative      = "native"
	appNoteKindBrowser     = "browser"
	defaultNoteDynerURL    = "https://dynapp.io"
	// Browser uninstalls are invisible to the agent; an app not opened for
	// this long drops out of the index.
	browserAppForgetAfter = 90 * 24 * time.Hour
)

type appNote struct {
	StoreID  string
	Name     string
	Origin   string
	Kind     string
	DynerURL string
	// SkillPath is the agent's local copy of the dynapp-developer skill
	// (SKILL.md), so a sandboxed coding agent can read it offline.
	SkillPath string
}

func noteDynerURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return defaultNoteDynerURL
	}
	return base
}

// appAgentNote describes one native install. It is deterministic (no version
// or timestamp), so a signed macOS bundle that contains it stays
// byte-identical across updates.
func appAgentNote(note appNote) []byte {
	base := noteDynerURL(note.DynerURL)
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "# %s (DynApp)\n\n", noteLine(note.Name))
	fmt.Fprintf(&buffer, "%s is a DynApp, installed on this computer as a desktop app from %s.\n\n", noteLine(note.Name), base)
	fmt.Fprintf(&buffer, "- App: `%s`\n", note.StoreID)
	fmt.Fprintf(&buffer, "- Address: %s/\n", strings.TrimRight(note.Origin, "/"))
	fmt.Fprintf(&buffer, "- Details and source: %s/api/v1/apps/%s\n\n", base, note.StoreID)
	buffer.Write(skillPointer(base, note.SkillPath, "this app"))
	buffer.WriteString("\nThe DynApp agent wrote this note and replaces it on every install.\n")
	return buffer.Bytes()
}

// skillPointer tells a coding agent where the workflow is and that changing
// an app needs the network, which sandboxes often block.
func skillPointer(base, skillPath, subject string) []byte {
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "To change, fix or customize %s, follow the dynapp-developer skill. Read it\n", subject)
	if skillPath != "" {
		fmt.Fprintf(&buffer, "before changing anything: a copy is at %s\n(the latest is %s/skill/SKILL.md).\n", skillPath, base)
	} else {
		fmt.Fprintf(&buffer, "before changing anything: %s/skill/SKILL.md.\n", base)
	}
	buffer.WriteString("Every DynApp is open source: the skill explains when the change is a new\n")
	buffer.WriteString("version of the app and when it is the user's own copy (a fork).\n\n")
	fmt.Fprintf(&buffer, "Changing an app downloads its source from %s and publishes it there, so it\n", host)
	fmt.Fprintf(&buffer, "needs network access to %s. If your sandbox blocks it, ask the user to allow it.\n", host)
	return buffer.Bytes()
}

func noteLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func noteCell(value string) string {
	return strings.ReplaceAll(noteLine(value), "|", "\\|")
}

type indexedApp struct {
	StoreID string `json:"storeId"`
	Name    string `json:"name"`
	Origin  string `json:"origin"`
	Kind    string `json:"kind,omitempty"`
	SeenAt  int64  `json:"seenAt,omitempty"`
}

// installedAppsMarkdown is the index a coding agent reads. Apps are sorted by
// name so an unchanged set of apps produces the same file.
func installedAppsMarkdown(apps []indexedApp, base, skillPath string) []byte {
	base = noteDynerURL(base)
	sorted := append([]indexedApp(nil), apps...)
	sort.Slice(sorted, func(i, j int) bool {
		left, right := strings.ToLower(sorted[i].Name), strings.ToLower(sorted[j].Name)
		if left != right {
			return left < right
		}
		return sorted[i].StoreID < sorted[j].StoreID
	})
	var buffer bytes.Buffer
	buffer.WriteString("# Apps installed on this computer from DynApp\n\n")
	fmt.Fprintf(&buffer, "These apps were installed from %s. Each one is open source. The user\n", base)
	buffer.WriteString("calls an app by its name; the App column is its id for the dynapp CLI.\n\n")
	if len(sorted) == 0 {
		buffer.WriteString("No apps are installed right now.\n\n")
	} else {
		buffer.WriteString("| Name | App | Address | Installed as |\n| --- | --- | --- | --- |\n")
		for _, app := range sorted {
			kind := "desktop app"
			if app.Kind == appNoteKindBrowser {
				kind = "web app (from the browser)"
			}
			fmt.Fprintf(&buffer, "| %s | `%s` | %s/ | %s |\n", noteCell(app.Name), app.StoreID, strings.TrimRight(app.Origin, "/"), kind)
		}
		buffer.WriteString("\n")
	}
	buffer.Write(skillPointer(base, skillPath, "one of these apps"))
	buffer.WriteString("\nThe DynApp agent maintains this file.\n")
	return buffer.Bytes()
}

func writeFileIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}

var installedAppsMu sync.Mutex

func readBrowserApps(dir string) []indexedApp {
	data, err := os.ReadFile(filepath.Join(dir, browserAppsFileName))
	if err != nil {
		return nil
	}
	var apps []indexedApp
	if json.Unmarshal(data, &apps) != nil {
		return nil
	}
	valid := apps[:0]
	for _, app := range apps {
		if validStoreID(app.StoreID) && strings.HasPrefix(app.Origin, "https://") {
			valid = append(valid, app)
		}
	}
	return valid
}

func writeBrowserApps(dir string, apps []indexedApp) error {
	sort.Slice(apps, func(i, j int) bool { return apps[i].StoreID < apps[j].StoreID })
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return err
	}
	return writeFileIfChanged(filepath.Join(dir, browserAppsFileName), append(data, '\n'))
}

// noteBrowserInstall records an app that runs installed from the browser on
// this computer. The store id, name and origin come from the verified
// handshake, never from the page.
func (s *Server) noteBrowserInstall(auth socketAuthentication) {
	if auth.development || !validStoreID(auth.storeID) || !strings.HasPrefix(auth.origin, "https://") {
		return
	}
	dir, err := codingAgentsDir()
	if err != nil {
		return
	}
	now := time.Now()
	installedAppsMu.Lock()
	apps := readBrowserApps(dir)
	found := false
	changed := true
	for index := range apps {
		if apps[index].StoreID != auth.storeID {
			continue
		}
		found = true
		// Record a reopen at most once a day.
		changed = apps[index].Name != auth.appName || apps[index].Origin != auth.origin || now.Sub(time.UnixMilli(apps[index].SeenAt)) > 24*time.Hour
		apps[index].Name, apps[index].Origin = auth.appName, auth.origin
		if changed {
			apps[index].SeenAt = now.UnixMilli()
		}
	}
	if !found {
		apps = append(apps, indexedApp{StoreID: auth.storeID, Name: auth.appName, Origin: auth.origin, Kind: appNoteKindBrowser, SeenAt: now.UnixMilli()})
	}
	if changed {
		_ = os.MkdirAll(dir, 0o755)
		_ = writeBrowserApps(dir, apps)
	}
	installedAppsMu.Unlock()
	s.refreshInstalledApps(context.Background())
}

// refreshInstalledApps rewrites installed-apps.md from the native installs
// and the recorded browser installs, keeps the skill copy and coding-agent
// lines current, and removes the per-app notes older agents wrote. A machine
// that never had an installed app is left alone.
func (s *Server) refreshInstalledApps(ctx context.Context) {
	dir, err := codingAgentsDir()
	if err != nil {
		return
	}
	s.mu.Lock()
	natives := append([]NativeApp(nil), s.Config.NativeApps...)
	base := s.Config.dynerBaseOrigin()
	s.mu.Unlock()
	defer removeLegacyAppNotes()

	installedAppsMu.Lock()
	apps := []indexedApp{}
	native := map[string]NativeApp{}
	for _, app := range natives {
		if !validStoreID(app.StoreID) || app.Path == "" {
			continue
		}
		if _, err := os.Stat(app.Path); err != nil {
			continue
		}
		native[app.StoreID] = app
		apps = append(apps, indexedApp{StoreID: app.StoreID, Name: app.Name, Origin: app.Origin, Kind: appNoteKindNative})
	}
	browser := readBrowserApps(dir)
	kept := browser[:0]
	for _, app := range browser {
		if time.Since(time.UnixMilli(app.SeenAt)) > browserAppForgetAfter {
			continue
		}
		kept = append(kept, app)
		if _, ok := native[app.StoreID]; !ok {
			apps = append(apps, app)
		}
	}
	if len(kept) != len(browser) {
		_ = writeBrowserApps(dir, kept)
	}
	_, indexErr := os.Stat(filepath.Join(dir, installedAppsIndexName))
	installedAppsMu.Unlock()
	if len(apps) == 0 && indexErr != nil {
		return
	}

	s.prepareCodingAgents(ctx)
	skill := localSkillPath()
	for _, app := range native {
		writeNativeAppNote(app, appNote{StoreID: app.StoreID, Name: app.Name, Origin: app.Origin, Kind: appNoteKindNative, DynerURL: base, SkillPath: skill})
	}
	installedAppsMu.Lock()
	defer installedAppsMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = writeFileIfChanged(filepath.Join(dir, installedAppsIndexName), installedAppsMarkdown(apps, base, skill))
	}
}

var legacyNoteStoreID = regexp.MustCompile("(?m)^- App: `[^`]+`$")

// removeLegacyAppNotes deletes the <Name>.dynapp.md notes, folder AGENTS.md
// and .skill copy that agents 0.1.59-0.1.61 wrote beside the installs. Only
// files with this agent's own content are removed.
func removeLegacyAppNotes() {
	dir, err := legacyAppNotesDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		switch {
		case entry.IsDir() && entry.Name() == ".skill":
			_ = os.RemoveAll(path)
		case !entry.IsDir() && strings.HasSuffix(entry.Name(), ".dynapp.md"):
			if data, err := os.ReadFile(path); err == nil && legacyNoteStoreID.Match(data) && bytes.Contains(data, []byte("The DynApp agent wrote this note")) {
				_ = os.Remove(path)
			}
		case !entry.IsDir() && entry.Name() == "AGENTS.md":
			if data, err := os.ReadFile(path); err == nil && bytes.HasPrefix(data, []byte("# DynApps installed on this computer")) {
				_ = os.Remove(path)
			}
		}
	}
	// Linux had a folder only for these notes.
	if strings.HasSuffix(filepath.ToSlash(dir), "dynapp/apps") {
		_ = os.Remove(dir)
	}
}

// loopbackPeer reports whether a carrier's remote address is this computer.
func loopbackPeer(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
