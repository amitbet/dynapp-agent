package shellagent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Coding agents learn that an installed app is a DynApp, and how to change
// it, from small Markdown notes the agent writes next to each install. A user
// says "change my Form & Fuel app"; the coding agent searches the disk for the
// name and lands on a note that names the app and points at the
// dynapp-developer skill. The notes hold facts only. The workflow lives in the
// skill (docs/agent-notes.md in the DynApp repository).

const (
	appNoteSuffix       = ".dynapp.md"
	appNoteKindNative   = "native"
	appNoteKindBrowser  = "browser"
	defaultNoteDynerURL = "https://dynapp.io"
)

type appNote struct {
	StoreID  string
	Name     string
	Origin   string
	Kind     string
	DynerURL string
}

func (note appNote) dynerURL() string {
	base := strings.TrimRight(strings.TrimSpace(note.DynerURL), "/")
	if base == "" {
		return defaultNoteDynerURL
	}
	return base
}

// appAgentNote is deterministic: it carries no version or timestamp, so a
// signed macOS bundle that contains it stays byte-identical across updates.
func appAgentNote(note appNote) []byte {
	base := note.dynerURL()
	installed := "as a desktop app"
	if note.Kind == appNoteKindBrowser {
		installed = "from the browser, as a web app (PWA)"
	}
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "# %s (DynApp)\n\n", noteLine(note.Name))
	fmt.Fprintf(&buffer, "%s is a DynApp, installed on this computer %s from %s.\n\n", noteLine(note.Name), installed, base)
	fmt.Fprintf(&buffer, "- App: `%s`\n", note.StoreID)
	fmt.Fprintf(&buffer, "- Address: %s/\n", strings.TrimRight(note.Origin, "/"))
	fmt.Fprintf(&buffer, "- Details and source: %s/api/v1/apps/%s\n\n", base, note.StoreID)
	buffer.WriteString("To change, fix or customize this app, use the dynapp-developer skill. If it\n")
	fmt.Fprintf(&buffer, "is not installed, read %s/skill/SKILL.md before changing anything.\n", base)
	buffer.WriteString("Every DynApp is open source: the skill explains when the change is a new\n")
	buffer.WriteString("version of this app and when it is the user's own copy (a fork).\n\n")
	buffer.WriteString("The DynApp agent wrote this note and replaces it on every install.\n")
	return buffer.Bytes()
}

// appNotesFolderReadme explains a folder of notes to an agent that lists it.
func appNotesFolderReadme(base string) []byte {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultNoteDynerURL
	}
	return []byte(fmt.Sprintf(`# DynApps installed on this computer

Each *%s file here describes one app installed from %s, named after the
app. To change, fix or customize one of them, use the dynapp-developer skill.
If it is not installed, read %s/skill/SKILL.md before changing anything.

The DynApp agent maintains this folder.
`, appNoteSuffix, base, base))
}

func noteLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

var noteStoreIDLine = regexp.MustCompile("(?m)^- App: `([^`]+)`$")

func noteStoreID(data []byte) string {
	match := noteStoreIDLine.FindSubmatch(data)
	if match == nil {
		return ""
	}
	return string(match[1])
}

var noteNameUnsafe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]+`)

// appNoteFileName is the app's display name, safe on every desktop OS, so a
// search for the name finds the note.
func appNoteFileName(name string) string {
	name = strings.TrimSpace(noteNameUnsafe.ReplaceAllString(name, " "))
	name = strings.Trim(name, ". ")
	if len(name) > 80 {
		name = strings.TrimSpace(name[:80])
	}
	if name == "" {
		name = "DynApp"
	}
	return name
}

var appNotesMu sync.Mutex

// writeAppNote writes dir/<name>.dynapp.md and the folder readme, and removes
// an older note for the same app (for example after a rename).
func writeAppNote(dir string, note appNote) error {
	if dir == "" || !validStoreID(note.StoreID) {
		return errors.New("app note needs a folder and a store id")
	}
	appNotesMu.Lock()
	defer appNotesMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFileIfChanged(filepath.Join(dir, "AGENTS.md"), appNotesFolderReadme(note.DynerURL)); err != nil {
		return err
	}
	base := appNoteFileName(note.Name)
	target := filepath.Join(dir, base+appNoteSuffix)
	if existing, err := os.ReadFile(target); err == nil {
		if owner := noteStoreID(existing); owner != "" && owner != note.StoreID {
			storeOwner, _, _ := strings.Cut(note.StoreID, "/")
			target = filepath.Join(dir, base+" ("+appNoteFileName(storeOwner)+")"+appNoteSuffix)
		}
	}
	if err := writeFileIfChanged(target, appAgentNote(note)); err != nil {
		return err
	}
	for _, path := range appNotePaths(dir, note.StoreID) {
		if !strings.EqualFold(path, target) {
			_ = os.Remove(path)
		}
	}
	return nil
}

// removeAppNote deletes every note in dir that belongs to storeID.
func removeAppNote(dir, storeID string) {
	if dir == "" || storeID == "" {
		return
	}
	appNotesMu.Lock()
	defer appNotesMu.Unlock()
	for _, path := range appNotePaths(dir, storeID) {
		_ = os.Remove(path)
	}
}

func appNotePaths(dir, storeID string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), appNoteSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if data, err := os.ReadFile(path); err == nil && noteStoreID(data) == storeID {
			paths = append(paths, path)
		}
	}
	return paths
}

func writeFileIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}

// noteBrowserInstall records an app that runs installed from the browser on
// this computer. The store id, name and origin come from the verified
// handshake, never from the page. A native install of the same app keeps its
// own note.
func (s *Server) noteBrowserInstall(auth socketAuthentication) {
	if auth.development || !validStoreID(auth.storeID) || !strings.HasPrefix(auth.origin, "https://") {
		return
	}
	if _, native := s.nativeApp(auth.storeID); native {
		return
	}
	s.mu.Lock()
	base := s.Config.dynerBaseOrigin()
	s.mu.Unlock()
	dir, err := appNotesDir()
	if err != nil {
		return
	}
	_ = writeAppNote(dir, appNote{StoreID: auth.storeID, Name: auth.appName, Origin: auth.origin, Kind: appNoteKindBrowser, DynerURL: base})
}

// backfillAppNotes writes notes for native apps installed before the agent
// wrote them. It never rewrites a signed macOS bundle, which would reset the
// app's camera grant: the in-bundle note arrives with the next install.
func (s *Server) backfillAppNotes() {
	s.mu.Lock()
	apps := append([]NativeApp(nil), s.Config.NativeApps...)
	base := s.Config.dynerBaseOrigin()
	s.mu.Unlock()
	for _, app := range apps {
		if !validStoreID(app.StoreID) || app.Path == "" {
			continue
		}
		if _, err := os.Stat(app.Path); err != nil {
			continue
		}
		writeNativeAppNotes(app, appNote{StoreID: app.StoreID, Name: app.Name, Origin: app.Origin, Kind: appNoteKindNative, DynerURL: base})
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
