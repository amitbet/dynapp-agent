//go:build windows

package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrashPathsRecyclesFilesOnWindows(t *testing.T) {
	dir := t.TempDir()
	names := []string{"one.txt", "with space.txt", "it's.txt", "ש.txt"}
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[i], []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := trashPath(paths[0]); err != nil {
		t.Fatalf("single trash: %v", err)
	}
	if err := trashPaths(append(paths[1:], folder)); err != nil {
		t.Fatalf("batch trash: %v", err)
	}
	for _, path := range append(paths, folder) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists after trash (err=%v)", path, err)
		}
	}
}
