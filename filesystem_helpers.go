package shellagent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

type boundedExecBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedExecBuffer) Write(value []byte) (int, error) {
	if remaining := buffer.limit - buffer.Len(); remaining > 0 {
		if len(value) > remaining {
			_, _ = buffer.Buffer.Write(value[:remaining])
		} else {
			_, _ = buffer.Buffer.Write(value)
		}
	}
	return len(value), nil
}

func userHome() string {
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir
	}
	home, _ := os.UserHomeDir()
	return home
}
func filesystemRoots() ([]map[string]string, error) {
	home := userHome()
	roots := []map[string]string{{"path": home, "label": "~"}}
	if runtime.GOOS == "windows" {
		for letter := 'A'; letter <= 'Z'; letter++ {
			drive := string(letter) + ":\\"
			if _, err := os.Stat(drive); err == nil {
				roots = append(roots, map[string]string{"path": drive, "label": string(letter) + ":"})
			}
		}
	} else {
		roots = append(roots, map[string]string{"path": "/", "label": "/"})
	}
	return roots, nil
}
func listDir(path string) (map[string]any, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, err
	}
	output := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		info, statErr := entry.Info()
		item := map[string]any{"name": entry.Name(), "isDir": entry.IsDir(), "isSymlink": entry.Type()&os.ModeSymlink != 0, "size": int64(0), "mtimeMs": int64(0), "mode": 0}
		if statErr == nil {
			item["size"] = info.Size()
			item["mtimeMs"] = info.ModTime().UnixMilli()
			item["mode"] = int(info.Mode())
		}
		output = append(output, item)
	}
	return map[string]any{"path": resolved, "entries": output}, nil
}
func statPath(path string) (any, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"isDir": info.IsDir(), "isSymlink": info.Mode()&os.ModeSymlink != 0, "size": info.Size(), "mtimeMs": info.ModTime().UnixMilli(), "mode": int(info.Mode())}, nil
}
func readText(path string, cap int) (map[string]any, error) {
	data, err := readAtMost(path, cap)
	if err != nil {
		return nil, err
	}
	info, _ := os.Stat(path)
	return map[string]any{"content": string(data), "size": info.Size(), "truncated": info.Size() > int64(len(data))}, nil
}
func readBase64(path string, cap int) (map[string]any, error) {
	data, err := readAtMost(path, cap)
	if err != nil {
		return nil, err
	}
	info, _ := os.Stat(path)
	return map[string]any{"base64": base64.StdEncoding.EncodeToString(data), "size": info.Size(), "truncated": info.Size() > int64(len(data))}, nil
}
func readAtMost(path string, cap int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, int64(cap)))
}
func readChunk(path string, offset, length int) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err = file.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(length)))
	if err != nil {
		return nil, err
	}
	return map[string]any{"base64": base64.StdEncoding.EncodeToString(data), "bytesRead": len(data), "size": info.Size(), "eof": int64(offset+len(data)) >= info.Size()}, nil
}
func writeChunk(path string, offset int64, data []byte, truncate bool) error {
	flags := os.O_WRONLY | os.O_CREATE
	if truncate {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o666)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteAt(data, offset)
	return err
}
func dirSize(root string) (map[string]any, error) {
	result := map[string]any{"bytes": int64(0), "files": 0, "dirs": 0, "truncated": false}
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			result["dirs"] = result["dirs"].(int) + 1
			return nil
		}
		info, statErr := entry.Info()
		if statErr == nil {
			result["bytes"] = result["bytes"].(int64) + info.Size()
			result["files"] = result["files"].(int) + 1
		}
		return nil
	})
	return result, err
}
func copyPath(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o777)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o666)
	})
}
func numberArg(args []any, index, fallback, maximum int) int {
	if len(args) <= index {
		return fallback
	}
	value, ok := args[index].(float64)
	if !ok || value < 0 {
		return fallback
	}
	if value > float64(maximum) {
		return maximum
	}
	return int(value)
}
func stringArg(args []any, index int) string {
	if len(args) <= index {
		return ""
	}
	value, _ := args[index].(string)
	return value
}
func boolArg(args []any, index int) bool {
	if len(args) <= index {
		return false
	}
	value, _ := args[index].(bool)
	return value
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func writeDirectLocalInfoCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Access-Control-Allow-Local-Network", "true")
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) handleDirectLocalInfo(w http.ResponseWriter, r *http.Request) {
	writeDirectLocalInfoCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status, err := s.lanStatus()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	environmentID := s.Config.EnvironmentID
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"version":                 1,
		"carrier":                 "webtransport",
		"endpoint":                status["directEndpoint"],
		"lanEndpoints":            status["lanEndpoints"],
		"serverCertificateHashes": status["serverCertificateHashes"],
		"environmentId":           environmentID,
		"running":                 status["running"],
	})
}
