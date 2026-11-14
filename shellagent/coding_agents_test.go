package shellagent

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func skillZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func skillServer(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skill/dynapp-developer.zip" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return server
}

func resetLocalSkillRefresh(t *testing.T) {
	localSkillRefresh.Lock()
	localSkillRefresh.at, localSkillRefresh.base = time.Time{}, ""
	localSkillRefresh.Unlock()
	t.Cleanup(func() {
		localSkillRefresh.Lock()
		localSkillRefresh.at, localSkillRefresh.base = time.Time{}, ""
		localSkillRefresh.Unlock()
	})
}

func TestReadSkillZipRejectsEscapes(t *testing.T) {
	good := skillZip(t, map[string]string{"dynapp-developer/SKILL.md": "# skill", "dynapp-developer/references/a.md": "a"})
	files, err := readSkillZip(good)
	if err != nil || string(files["SKILL.md"]) != "# skill" || string(files["references/a.md"]) != "a" {
		t.Fatalf("files = %v, %v", files, err)
	}
	for _, bad := range []map[string]string{
		{"dynapp-developer/SKILL.md": "x", "dynapp-developer/../../evil": "x"},
		{"dynapp-developer/SKILL.md": "x", "other/SKILL.md": "x"},
		{"/dynapp-developer/SKILL.md": "x"},
		{"dynapp-developer/C:/evil": "x", "dynapp-developer/SKILL.md": "x"},
		{"dynapp-developer/references/a.md": "no SKILL.md"},
	} {
		if _, err := readSkillZip(skillZip(t, bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestRefreshLocalSkillReplacesTheCopy(t *testing.T) {
	notes := t.TempDir()
	server := skillServer(t, skillZip(t, map[string]string{"dynapp-developer/SKILL.md": "v2", "dynapp-developer/references/b.md": "b"}))
	stale := filepath.Join(localSkillDir(notes), "references", "old.md")
	_ = os.MkdirAll(filepath.Dir(stale), 0o755)
	_ = os.WriteFile(stale, []byte("old"), 0o644)
	if err := refreshLocalSkill(context.Background(), server.Client(), server.URL, notes); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(localSkillDir(notes), "SKILL.md")); string(data) != "v2" {
		t.Fatalf("SKILL.md = %q", data)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale reference survived")
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not a zip")) }))
	defer broken.Close()
	if err := refreshLocalSkill(context.Background(), broken.Client(), broken.URL, notes); err == nil {
		t.Fatal("a broken archive was accepted")
	}
	if data, _ := os.ReadFile(filepath.Join(localSkillDir(notes), "SKILL.md")); string(data) != "v2" {
		t.Fatal("a failed refresh removed the existing copy")
	}
}

func TestUpsertMarkedBlockKeepsTheRestOfTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(file, []byte("# Mine\nKeep this."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upsertMarkedBlock(file, "first"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(mustRead(t, file), []byte("\nAfter.\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upsertMarkedBlock(file, "second"); err != nil {
		t.Fatal(err)
	}
	want := "# Mine\nKeep this.\n\n<!-- dynapp -->\nsecond\n<!-- /dynapp -->\n\nAfter.\n"
	if got := string(mustRead(t, file)); got != want {
		t.Fatalf("file =\n%q\nwant\n%q", got, want)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", info.Mode())
	}
	empty := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := upsertMarkedBlock(empty, "only"); err != nil || string(mustRead(t, empty)) != "<!-- dynapp -->\nonly\n<!-- /dynapp -->\n" {
		t.Fatalf("new file = %q, %v", mustRead(t, empty), err)
	}
}

func TestCodingAgentPointersOnlyForInstalledAgents(t *testing.T) {
	home := t.TempDir()
	notes := filepath.Join(home, "notes")
	if err := writeCodingAgentPointers(home, "", notes); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".codex", ".claude"} {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatalf("created %s for an agent that is not set up", dir)
		}
	}
	codexHome := filepath.Join(home, "codex-home")
	_ = os.MkdirAll(codexHome, 0o755)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	if err := writeCodingAgentPointers(home, codexHome, notes); err != nil {
		t.Fatal(err)
	}
	line := "To change an app the user installed, first check " + filepath.Join(notes, "installed-apps.md") + "."
	for _, file := range []string{filepath.Join(codexHome, "AGENTS.md"), filepath.Join(home, ".claude", "CLAUDE.md")} {
		if !strings.Contains(string(mustRead(t, file)), line) {
			t.Fatalf("%s lacks the pointer:\n%s", file, mustRead(t, file))
		}
	}
}

func mustRead(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
