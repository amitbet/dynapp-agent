package shellagent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacClipboardScriptQuotesPaths(t *testing.T) {
	got := macClipboardScript([]string{`/Users/amit/My "File".txt`})
	if got != `set the clipboard to POSIX file "/Users/amit/My \"File\".txt"` {
		t.Fatalf("unexpected AppleScript: %s", got)
	}
	many := macClipboardScript([]string{"/tmp/a.txt", "/tmp/b.txt"})
	if !strings.Contains(many, "POSIX file \"/tmp/a.txt\"") || !strings.Contains(many, "POSIX file \"/tmp/b.txt\"") {
		t.Fatalf("unexpected multi-file AppleScript: %s", many)
	}
}

func TestClipboardAllocateWriteStaysInAppState(t *testing.T) {
	server := &Server{StateDir: t.TempDir()}
	allocated, err := server.handleClipboardRPC(message{
		AppID:  "remote-control",
		Method: "allocate",
		Args:   []any{map[string]any{"name": "note.txt", "size": float64(5), "digest": strings.Repeat("ab", 32), "mimeType": "text/plain"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := allocated.(map[string]any)
	path := record["path"].(string)
	if filepath.Dir(path) != filepath.Join(server.StateDir, "clipboard-files", "remote-control") {
		t.Fatalf("clipboard file escaped app state: %s", path)
	}
	if _, err := server.handleClipboardRPC(message{
		AppID:  "remote-control",
		Method: "write",
		Args:   []any{path, float64(0), base64.StdEncoding.EncodeToString([]byte("hello"))},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("wrote %q", got)
	}
	if _, err := server.handleClipboardRPC(message{
		AppID:  "other-app",
		Method: "publish",
		Args:   []any{[]any{path}},
	}); err == nil {
		t.Fatal("expected other apps to be refused")
	}
}

func TestClipboardPromiseStreamsToRequestedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	server := &Server{promises: map[string]*clipboardPromise{}}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	server.promises["promise-1"] = &clipboardPromise{id: "promise-1", appID: "remote-control", path: path, file: file, size: 5}
	if _, err := server.handleClipboardRPC(message{AppID: "remote-control", Method: "writePromise", Args: []any{"promise-1", float64(0), base64.StdEncoding.EncodeToString([]byte("hello"))}}); err != nil {
		t.Fatal(err)
	}
	// Complete the file directly here. The native helper completion transport is
	// covered by its integration test and is intentionally absent in this unit.
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("wrote %q", got)
	}
}

func TestClipboardPromiseCanBeRequestedByAnotherApp(t *testing.T) {
	directory := t.TempDir()
	server := &Server{promises: map[string]*clipboardPromise{
		"promise-1": {id: "promise-1", appID: "remote-control", sourceID: "source-1", name: "movie.mp4", size: 42},
	}}
	result, err := server.handleClipboardRPCWithSocket(nil, message{
		AppID: "commander", Method: "requestPromise", Args: []any{"promise-1", directory},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := result.(map[string]any)
	path := record["path"].(string)
	if filepath.Dir(path) != directory || filepath.Base(path) != "movie.mp4" {
		t.Fatalf("unexpected destination: %s", path)
	}
	if record["name"] != "movie.mp4" || record["size"] != int64(42) {
		t.Fatalf("unexpected promise metadata: %#v", record)
	}
	if server.promises["promise-1"].file == nil {
		t.Fatal("request did not open destination")
	}
	_ = server.promises["promise-1"].file.Close()
}

func TestClipboardPromiseAcceptsBinaryChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary.bin")
	server := &Server{promises: map[string]*clipboardPromise{}}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	server.promises["promise-binary"] = &clipboardPromise{id: "promise-binary", appID: "remote-control", path: path, file: file, size: 6}
	if _, err := server.handleClipboardRPCWithSocket(nil, message{
		AppID: "remote-control", Method: "writePromiseBinary", Args: []any{"promise-binary", float64(0)},
	}, []byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("wrote %q", got)
	}
}

func TestClipboardDeferredSizeAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "failed"}[failed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			server := &Server{promises: map[string]*clipboardPromise{"p": {id: "p", appID: "source", path: path, file: file, size: -1}}}
			rpc := func(app, method string, args ...any) (any, error) {
				return server.handleClipboardRPC(message{AppID: app, Method: method, Args: args})
			}
			if _, err = rpc("other", "setPromiseSize", "p", float64(5)); err == nil {
				t.Fatal("another app changed the size")
			}
			for _, size := range []float64{-1, 1.5, 1 << 54} {
				if _, err = rpc("source", "setPromiseSize", "p", size); err == nil {
					t.Fatal("accepted invalid size", size)
				}
			}
			if _, err = rpc("source", "setPromiseSize", "p", float64(5)); err != nil {
				t.Fatal(err)
			}
			if _, err = rpc("source", "setPromiseSize", "p", float64(6)); err == nil {
				t.Fatal("changed a known size")
			}
			if _, err = file.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			args := []any{"p"}
			if failed {
				args = append(args, map[string]any{"error": "remote disconnected"})
			}
			_, err = rpc("source", "finishPromise", args...)
			if failed {
				if err == nil || !strings.Contains(err.Error(), "remote disconnected") {
					t.Fatal(err)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed partial file remains")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(server.promises) != 0 {
				t.Fatal("completed promise remains active")
			}
		})
	}
}
