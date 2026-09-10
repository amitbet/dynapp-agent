package shellagent

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateZipAndWatchSnapshot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "nested", "hello.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := watchSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "bundle.zip")
	if err := createZip([]string{source}, archivePath, map[string]any{"includeBaseDir": true}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	found := false
	for _, entry := range archive.File {
		if entry.Name == "bundle/nested/hello.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("archive entries = %#v", archive.File)
	}
	if err := os.WriteFile(file, []byte("changed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := watchSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	if before["fingerprint"] == after["fingerprint"] {
		t.Fatal("watch snapshot did not change")
	}
}

func TestImportedFilesAreChunkedIntoAppScopedState(t *testing.T) {
	server := &Server{StateDir: t.TempDir()}
	data := []byte("browser attachment")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	allocated, err := server.handleFileImportRPC(message{AppID: "write", Method: "allocate", Args: []any{map[string]any{"name": "note.txt", "mimeType": "text/plain", "size": float64(len(data)), "digest": digest}}})
	if err != nil {
		t.Fatal(err)
	}
	path := allocated.(map[string]any)["path"].(string)
	if _, err := server.handleFileImportRPC(message{AppID: "write", Method: "write", Args: []any{path, 0.0, base64.StdEncoding.EncodeToString(data)}}); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(path); err != nil || string(actual) != string(data) {
		t.Fatalf("import = %q, %v", actual, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	_ = os.WriteFile(outside, []byte("safe"), 0o600)
	if _, err := server.handleFileImportRPC(message{AppID: "write", Method: "write", Args: []any{outside, 0.0, base64.StdEncoding.EncodeToString([]byte("bad"))}}); err == nil {
		t.Fatal("outside import path was accepted")
	}
}

func TestFilesystemDesktopCapabilityMapping(t *testing.T) {
	for method, expected := range map[string]string{
		"readChunkBinary":  "fs.readChunk",
		"writeChunkBinary": "fs.writeChunk",
		"packZip":          "fs.pack",
		"openWithOptions":  "fs.openWith",
		"watchSnapshot":    "fs.watch",
		"openWith":         "fs.openWith",
		"readText":         "fs.readText",
		"execFile":         "fs.execFile",
	} {
		if actual := filesystemCapability(method); actual != expected {
			t.Fatalf("%s capability = %s, want %s", method, actual, expected)
		}
	}
}
