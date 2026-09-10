package shellagent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaticLivePreviewReloadsWhenBuiltContentChanges(t *testing.T) {
	root := t.TempDir()
	content := filepath.Join(root, "content")
	if err := os.MkdirAll(content, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(content, "index.html")
	if err := os.WriteFile(indexPath, []byte("<!doctype html><body>before</body>"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := &Server{}
	t.Cleanup(agent.closeLivePreviews)
	url, err := agent.startLivePreview(root, false)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "/.dynapp-live-version") {
		t.Fatalf("live reload client missing from %q", body)
	}
	versionResponse, _ := http.Get(url + ".dynapp-live-version")
	before, _ := io.ReadAll(versionResponse.Body)
	_ = versionResponse.Body.Close()
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(indexPath, []byte("<!doctype html><body>after and longer</body>"), 0o600); err != nil {
		t.Fatal(err)
	}
	versionResponse, _ = http.Get(url + ".dynapp-live-version")
	after, _ := io.ReadAll(versionResponse.Body)
	_ = versionResponse.Body.Close()
	if string(before) == string(after) {
		t.Fatal("content version did not change after rebuild")
	}
}

func TestStartLivePreviewUsesDeclaredViteServer(t *testing.T) {
	devServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("source preview"))
	}))
	defer devServer.Close()

	root := t.TempDir()
	dynapp := fmt.Sprintf(`{"authoring":{"mode":"vite","devUrl":%q}}`, devServer.URL)
	if err := os.WriteFile(filepath.Join(root, "dynapp.json"), []byte(dynapp), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := &Server{}
	t.Cleanup(agent.closeLivePreviews)
	got, err := agent.startLivePreview(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != devServer.URL {
		t.Fatalf("preview URL = %q, want declared Vite URL %q", got, devServer.URL)
	}
}

func TestReadViteAuthoringRejectsNonLoopbackURL(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dynapp.json"), []byte(
		`{"authoring":{"mode":"vite","devCommand":"npm run dev","devUrl":"https://example.com"}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readViteAuthoring(root); ok {
		t.Fatal("non-loopback authoring URL was accepted")
	}
}
