package shellagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type failingReadCloser struct{ sent bool }

func (reader *failingReadCloser) Read(buffer []byte) (int, error) {
	if reader.sent {
		return 0, errors.New("injected read failure")
	}
	reader.sent = true
	return copy(buffer, []byte("partial")), nil
}
func (*failingReadCloser) Close() error { return nil }

type failingNetworkFiles struct{ *localNetworkFiles }

func (*failingNetworkFiles) OpenRead(string) (io.ReadCloser, int64, error) {
	return &failingReadCloser{}, 100, nil
}

type localNetworkFiles struct{ root string }

func (files *localNetworkFiles) resolve(value string) string {
	cleaned, err := remotePath(value)
	if err != nil {
		return filepath.Join(files.root, "invalid")
	}
	return filepath.Join(files.root, filepath.FromSlash(cleaned))
}
func (files *localNetworkFiles) List(value string) (any, error) {
	entries, err := os.ReadDir(files.resolve(value))
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, entryValue(entry.Name(), info.IsDir(), info.Mode()&os.ModeSymlink != 0, info.Size(), info.ModTime(), info.Mode()))
	}
	return map[string]any{"path": value, "entries": result}, nil
}
func (files *localNetworkFiles) Stat(value string) (any, error) {
	info, err := os.Stat(files.resolve(value))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return entryValue(filepath.Base(value), info.IsDir(), info.Mode()&os.ModeSymlink != 0, info.Size(), info.ModTime(), info.Mode()), nil
}
func (files *localNetworkFiles) OpenRead(value string) (io.ReadCloser, int64, error) {
	file, err := os.Open(files.resolve(value))
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}
func (files *localNetworkFiles) OpenReadAt(value string, offset int64) (io.ReadCloser, int64, error) {
	file, size, err := files.OpenRead(value)
	if err != nil {
		return nil, 0, err
	}
	if offset > size {
		offset = size
	}
	if _, err := file.(*os.File).Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, size, nil
}
func (files *localNetworkFiles) OpenWrite(value string) (io.WriteCloser, error) {
	target := files.resolve(value)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	return os.Create(target)
}
func (files *localNetworkFiles) Read(value string, limit int64) ([]byte, int64, error) {
	reader, size, err := files.OpenRead(value)
	if err != nil {
		return nil, 0, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	return data, size, err
}
func (files *localNetworkFiles) Write(value string, data []byte) error {
	writer, err := files.OpenWrite(value)
	if err != nil {
		return err
	}
	if _, err = writer.Write(data); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}
func (files *localNetworkFiles) Mkdir(value string) error {
	return os.MkdirAll(files.resolve(value), 0o755)
}
func (files *localNetworkFiles) Remove(value string) error { return os.RemoveAll(files.resolve(value)) }
func (files *localNetworkFiles) Rename(from, to string) error {
	return os.Rename(files.resolve(from), files.resolve(to))
}
func (files *localNetworkFiles) Close() error { return nil }

func TestNetworkCopiesStreamDirectoriesAndReportProgress(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 700*1024)
	for index := range content {
		content[index] = byte(index % 251)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "large.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	client := &localNetworkFiles{root: t.TempDir()}
	events := []map[string]any{}
	progress := &copyProgress{total: int64(len(content)), publish: func(event map[string]any) { events = append(events, event) }}
	if err := copyLocalToNetwork(client, source, "/uploaded", progress); err != nil {
		t.Fatal(err)
	}
	if progress.bytes != int64(len(content)) || len(events) < 2 {
		t.Fatalf("progress = %d, events = %d", progress.bytes, len(events))
	}
	destination := filepath.Join(t.TempDir(), "downloaded")
	progress = &copyProgress{total: int64(len(content)), publish: func(map[string]any) {}}
	if err := copyNetworkToLocal(client, "/uploaded", destination, progress); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "nested", "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatal("streamed directory copy changed file content")
	}
}

func TestNetworkDownloadDoesNotReplaceTargetAfterReadFailure(t *testing.T) {
	remoteRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteRoot, "source.txt"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &failingNetworkFiles{localNetworkFiles: &localNetworkFiles{root: remoteRoot}}
	if err := copyNetworkToLocal(client, "/source.txt", target, &copyProgress{publish: func(map[string]any) {}}); err == nil {
		t.Fatal("expected the injected read failure")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("target was replaced with %q", content)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".dynapp-download-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary downloads were not removed: %v", matches)
	}
}

func TestFTPControlArgumentsRejectCRLFAndContainLocalCopies(t *testing.T) {
	if _, err := ftpControlArgument("ftp.example.test\nPORT 21", "host"); err == nil {
		t.Fatal("CRLF host was accepted")
	}
	if _, err := remotePath("/ok/\rSTOR x"); err == nil {
		t.Fatal("CRLF remote path was accepted")
	}
	root := t.TempDir()
	joined, err := joinLocalCopyPath(root, "nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(joined) != filepath.Join(root, "nested") {
		t.Fatalf("joined = %s", joined)
	}
	if _, err := joinLocalCopyPath(root, "../escape.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := joinLocalCopyPath(root, "C:/windows"); err == nil {
		t.Fatal("drive-letter escape was accepted")
	}
}
