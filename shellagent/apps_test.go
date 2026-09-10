package shellagent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishAppProjectUploadsSourceAndRunnable(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app.json", `{"id":"demo","name":"Demo","version":"1.2.3","updates":{"storeId":"user-1/demo"}}`)
	write("package.json", `{"version":"1.2.3"}`)
	write("content/index.html", "<!doctype html><title>Demo</title>")
	write("content/version.json", `{"version":"1.2.3"}`)
	write("src/main.js", "console.log('source')")
	write("node_modules/ignored.js", "ignored")
	var published map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing bearer token")
		}
		switch r.URL.Path {
		case "/api/v1/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": "user-1"}})
		case "/api/v1/source/blobs/check":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			blobs := request["blobs"].([]any)
			missing := make([]any, len(blobs))
			for index, blob := range blobs {
				missing[index] = blob.(map[string]any)["digest"]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"missing": missing})
		case "/api/v1/source/packs":
			if r.Header.Get("Content-Encoding") != "gzip" {
				t.Errorf("source pack is not compressed")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/revisions":
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(r.FormValue("sourceManifest")), &published)
			file, _, err := r.FormFile("content")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			archiveBytes, _ := io.ReadAll(file)
			archive, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
			if err != nil {
				t.Fatal(err)
			}
			foundNotes := false
			for _, entry := range archive.File {
				if entry.Name == "VERSION_NOTES.txt" {
					foundNotes = true
				}
			}
			if !foundNotes {
				t.Error("release notes missing from runnable")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"id": "user-1/demo"}, "revision": map[string]any{"id": "rev-1", "version": "1.2.4"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	agent := &Server{StateDir: t.TempDir(), AccountToken: "test-token", Config: Config{DynerBaseURL: server.URL}}
	result, err := agent.publishAppProject(context.Background(), map[string]any{"projectRoot": root, "bumpVersion": "patch", "releaseNotes": "Published from PWA"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["version"] != "1.2.4" {
		t.Fatalf("unexpected result: %#v", result)
	}
	files := published["files"].([]any)
	paths := map[string]bool{}
	for _, item := range files {
		paths[item.(map[string]any)["path"].(string)] = true
	}
	if !paths["src/main.js"] || paths["content/index.html"] || paths["node_modules/ignored.js"] {
		t.Fatalf("unexpected source manifest: %#v", paths)
	}
	manifest, _, _ := readPublishJSON(filepath.Join(root, "app.json"))
	if manifest["version"] != "1.2.4" {
		t.Fatalf("version bump was not kept: %#v", manifest)
	}
}

func TestCollectPublishSourceIncludesContentWhenDeclared(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("content/main.js", "authored")
	write("src/ignored.js", "other")
	files, err := collectPublishSource(root, true)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range files {
		paths[file.Path] = true
	}
	if !paths["content/main.js"] || !paths["src/ignored.js"] {
		t.Fatalf("declared content source was omitted: %#v", paths)
	}
}

func TestOpenAppDraftMaterializesSourceAndStartsLivePreview(t *testing.T) {
	files := map[string]string{
		"app.json":     "{\n  \"id\": \"remote-control\",\n  \"name\": \"Remote Control\"\n}\n",
		"package.json": "{\n  \"scripts\": { \"build\": \"node build.mjs\" }\n}\n",
	}
	blobs := map[string][]byte{}
	snapshotFiles := []sourceSnapshotFile{}
	for path, content := range files {
		data := []byte(content)
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		blobs[digest] = data
		snapshotFiles = append(snapshotFiles, sourceSnapshotFile{Path: path, Digest: digest, Size: int64(len(data)), Mode: 420})
	}
	pack := encodeSourcePack(snapshotFiles, blobs)
	var snapshotDocument []byte
	runnable := zipFiles(t, map[string]string{
		"index.html":     "<!doctype html><title>Published live app</title>\n",
		"assets/main.js": "document.body.dataset.ready = 'true';\n",
		"version.json":   "{\"version\":\"1.10\"}\n",
	})
	var packURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/apps/amit-bet/remote-control":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"latestRevision": map[string]any{
					"id":                     "rev_1",
					"version":                "1.10",
					"source_url":             serverURL(r, "/source-snapshot"),
					"source_snapshot_digest": "sha256:" + hex.EncodeToString(sumOf(snapshotDocument)),
					"runnable_url":           serverURL(r, "/content.zip"),
					"runnable_sha256":        hex.EncodeToString(sumOf(runnable)),
				},
			})
		case r.URL.Path == "/source-snapshot":
			_, _ = w.Write(snapshotDocument)
		case r.URL.Path == "/source-pack":
			w.Header().Set("Content-Type", "application/vnd.dyner.source-pack")
			_, _ = w.Write(pack)
		case r.URL.Path == "/content.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(runnable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	packURL = server.URL + "/source-pack"
	snapshotDocument, _ = json.Marshal(map[string]any{
		"schemaVersion": 1,
		"algorithm":     "sha256",
		"packUrl":       packURL,
		"files":         snapshotFiles,
	})

	agent := &Server{StateDir: t.TempDir(), Config: Config{DynerBaseURL: server.URL}}
	t.Cleanup(agent.closeLivePreviews)
	result, err := agent.handleAppsRPC(context.Background(), message{
		Method: "draft",
		Args:   []any{map[string]any{"appId": "remote-control", "storeId": "amit-bet/remote-control", "openPreview": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := result.(map[string]any)
	projectRoot, _ := payload["projectRoot"].(string)
	if projectRoot == "" {
		t.Fatal(payload)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "app.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "content", "index.html")); err != nil {
		t.Fatalf("published runnable was not materialized: %v", err)
	}
	if payload["live"] != true || payload["livePreviewUrl"] == nil {
		t.Fatalf("expected live preview URL, got %#v", payload)
	}
	drafts, err := agent.handleAppsRPC(context.Background(), message{Method: "listDrafts"})
	if err != nil {
		t.Fatal(err)
	}
	draftList, _ := drafts.([]any)
	if len(draftList) != 1 || draftList[0].(map[string]any)["projectRoot"] != projectRoot {
		t.Fatalf("draft handoff project was not discoverable: %#v", drafts)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "dynapp.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "source-marker.txt"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(projectRoot, "content")); err != nil {
		t.Fatal(err)
	}
	reused, err := agent.handleAppsRPC(context.Background(), message{
		Method: "draft",
		Args:   []any{map[string]any{"appId": "remote-control", "storeId": "amit-bet/remote-control", "openPreview": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.(map[string]any)["reused"] != true {
		t.Fatalf("expected existing draft reuse, got %#v", reused)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "content", "index.html")); err != nil {
		t.Fatalf("missing runnable was not repaired: %v", err)
	}
	if marker, err := os.ReadFile(filepath.Join(projectRoot, "source-marker.txt")); err != nil || string(marker) != "preserve" {
		t.Fatalf("draft source was not preserved: %q, %v", marker, err)
	}
}

func TestAppsRPCRequiresAppsCapabilities(t *testing.T) {
	if required := rpcCapability(message{Service: "apps", Method: "draft"}); required != "apps.manage" {
		t.Fatalf("apps RPC capability = %q", required)
	}
	if required := rpcCapability(message{Service: "apps", Method: "publish"}); required != "apps.publish" {
		t.Fatalf("apps publish capability = %q", required)
	}
	if required := rpcCapability(message{Service: "externalOpen", Method: "takeData"}); required != "externalOpen.files" {
		t.Fatalf("externalOpen capability = %q", required)
	}
}

func TestOpenAppDraftRequiresFullStoreID(t *testing.T) {
	agent := &Server{StateDir: t.TempDir()}
	if _, err := agent.openAppDraft(context.Background(), map[string]any{"appId": "remote-control"}); err == nil {
		t.Fatal("expected a bare app id to be rejected")
	}
}

func TestSourceSnapshotDigestMustMatchDocument(t *testing.T) {
	document := []byte(`{"schemaVersion":1}`)
	sum := sha256.Sum256(document)
	if !snapshotDigestMatches("sha256:"+hex.EncodeToString(sum[:]), document) {
		t.Fatal("matching digest was rejected")
	}
	if snapshotDigestMatches("sha256:"+hex.EncodeToString(sum[:]), []byte(`{"schemaVersion":2}`)) {
		t.Fatal("mismatched document was accepted")
	}
	if snapshotDigestMatches("", document) {
		t.Fatal("empty digest was accepted")
	}
}

func TestScopedBearerHeadersOnlyReachDynerBaseOrigin(t *testing.T) {
	headers := scopedBearerHeaders("https://dynapp.io", "secret")
	if headers.forURL("https://dynapp.io/api/v1/apps/a/b").Get("Authorization") != "Bearer secret" {
		t.Fatal("bearer token missing for the Dyner base origin")
	}
	if headers.forURL("https://cdn.example/content.zip").Get("Authorization") != "" {
		t.Fatal("bearer token leaked to a foreign origin")
	}
	if headers.forURL("https://evil.dynapp.io/api").Get("Authorization") != "" {
		t.Fatal("bearer token leaked to a sibling host")
	}
}

func TestRunnableArchiveVerifiesSHA256WhenProvided(t *testing.T) {
	archive := zipFiles(t, map[string]string{"index.html": "<!doctype html>"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	root := t.TempDir()
	if err := materializeRunnableArchive(server.Client(), server.URL, hex.EncodeToString(sumOf([]byte("other"))), filepath.Join(root, "content"), scopedHeaders{}); err == nil {
		t.Fatal("expected mismatched runnable_sha256 to be rejected")
	}
	if err := materializeRunnableArchive(server.Client(), server.URL, hex.EncodeToString(sumOf(archive)), filepath.Join(root, "content"), scopedHeaders{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnableArchiveRejectsPathsOutsideContent(t *testing.T) {
	archive := zipFiles(t, map[string]string{
		"index.html":    "<!doctype html>",
		"../escaped.js": "unsafe",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	root := t.TempDir()
	err := materializeRunnableArchive(server.Client(), server.URL, "", filepath.Join(root, "content"), scopedHeaders{})
	if err == nil {
		t.Fatal("expected unsafe runnable archive to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(root, "escaped.js")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe archive escaped destination: %v", statErr)
	}
}

func serverURL(r *http.Request, path string) string {
	scheme := "http"
	return scheme + "://" + r.Host + path
}

func sumOf(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func zipFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
