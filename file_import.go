package shellagent

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxImportedFileBytes = 64 * 1024 * 1024

var importDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *Server) handleFileImportRPC(request message) (any, error) {
	if request.Method == "write" {
		return s.writeImportedFileChunk(request)
	}
	if request.Method != "allocate" {
		return nil, errors.New("unsupported file-import method")
	}
	value := objectArg(request.Args, 0)
	name := filepath.Base(strings.ReplaceAll(stringValue(value["name"]), "\x00", ""))
	digest := strings.ToLower(stringValue(value["digest"]))
	size := 0
	if raw, ok := value["size"].(float64); ok {
		size = int(raw)
	} else if raw, ok := value["size"].(int); ok {
		size = raw
	}
	if name == "" || name == "." || name == string(filepath.Separator) || !importDigestPattern.MatchString(digest) || size < 0 || size > maxImportedFileBytes {
		return nil, errors.New("imported file metadata is invalid")
	}
	root := s.StateDir
	if root == "" {
		root, _ = DefaultStateDir()
	}
	directory := filepath.Join(root, "file-imports", safeName(request.AppID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, digest[:16]+"-"+name)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(size) {
		return map[string]any{"path": path, "name": name, "size": size, "mimeType": stringValue(value["mimeType"]), "existing": true}, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "name": name, "size": size, "mimeType": stringValue(value["mimeType"]), "existing": false, "maxChunkBytes": 240 * 1024}, nil
}

func (s *Server) writeImportedFileChunk(request message) (any, error) {
	path := stringArg(request.Args, 0)
	offset := numberArg(request.Args, 1, 0, maxImportedFileBytes)
	raw, err := base64.StdEncoding.DecodeString(stringArg(request.Args, 2))
	if err != nil || len(raw) > 240*1024 {
		return nil, errors.New("imported file chunk is invalid")
	}
	root := s.StateDir
	if root == "" {
		root, _ = DefaultStateDir()
	}
	directory := filepath.Join(root, "file-imports", safeName(request.AppID))
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(directory, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("imported file path is outside app state")
	}
	file, err := os.OpenFile(absolute, os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if offset+len(raw) > maxImportedFileBytes {
		return nil, errors.New("imported file exceeds the size limit")
	}
	written, err := file.WriteAt(raw, int64(offset))
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": absolute, "offset": offset, "bytesWritten": written}, nil
}
