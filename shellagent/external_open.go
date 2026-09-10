package shellagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) handleExternalOpenHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// Same rules as the settings API: loopback peer and Host, own origin for
	// browsers (with the CSRF token), JSON body for everyone.
	if !s.guardSettingsRequest(w, r, true) {
		return
	}
	reader := http.MaxBytesReader(w, r.Body, 64*1024)
	defer reader.Close()
	var payload struct {
		AppID string   `json:"appId"`
		Paths []string `json:"paths"`
	}
	if json.NewDecoder(reader).Decode(&payload) != nil || safeName(payload.AppID) != payload.AppID || len(payload.Paths) > 64 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	valid := []string{}
	for _, value := range payload.Paths {
		path := filepath.Clean(value)
		if filepath.IsAbs(path) {
			valid = append(valid, path)
		}
	}
	s.externalMu.Lock()
	if s.externalOpens == nil {
		s.externalOpens = map[string][]string{}
	}
	s.externalOpens[payload.AppID] = append(s.externalOpens[payload.AppID], valid...)
	s.externalMu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "queued": len(valid)})
}
func (s *Server) handleExternalOpenRPC(ctx context.Context, request message) (any, error) {
	_ = ctx
	if request.Method != "takeData" || request.AppID == "" {
		return nil, errors.New("unsupported external-open method")
	}
	// The launcher queues by the app slug; paired sockets are bound to the
	// full store id. Drain both spellings of the bound id.
	keys := []string{request.AppID}
	if slug := storeSlug(request.AppID); slug != request.AppID {
		keys = append(keys, slug)
	}
	var paths []string
	s.externalMu.Lock()
	for _, key := range keys {
		paths = append(paths, s.externalOpens[key]...)
		delete(s.externalOpens, key)
	}
	s.externalMu.Unlock()
	for _, key := range keys {
		persisted, _ := takePersistedExternalOpens(s.StateDir, key)
		paths = append(paths, persisted...)
	}
	records := []map[string]any{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			records = append(records, map[string]any{"path": path, "name": filepath.Base(path), "directory": true})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxReadBytes {
			continue
		}
		records = append(records, map[string]any{"path": path, "name": filepath.Base(path), "extension": strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), "mimeType": http.DetectContentType(data), "base64": base64.StdEncoding.EncodeToString(data), "content": string(data)})
	}
	return records, nil
}
func QueueExternalOpenFallback(stateDir, appID string, paths []string) error {
	if safeName(appID) != appID || appID == "" {
		return errors.New("app id is invalid")
	}
	root := stateDir
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return err
		}
	}
	dir := filepath.Join(root, "external-open", appID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	valid := []string{}
	for _, value := range paths {
		value = filepath.Clean(value)
		if filepath.IsAbs(value) {
			valid = append(valid, value)
		}
	}
	data, _ := json.Marshal(map[string]any{"paths": valid, "queuedAt": time.Now().UnixMilli()})
	file, err := os.CreateTemp(dir, ".open-*.json")
	if err != nil {
		return err
	}
	name := file.Name()
	_ = file.Chmod(0o600)
	if _, err = file.Write(data); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
func takePersistedExternalOpens(stateDir, appID string) ([]string, error) {
	root := stateDir
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(root, "external-open", safeName(appID))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		data, readErr := os.ReadFile(file)
		if readErr == nil {
			var payload struct {
				Paths []string `json:"paths"`
			}
			if json.Unmarshal(data, &payload) == nil {
				paths = append(paths, payload.Paths...)
			}
		}
		_ = os.Remove(file)
	}
	return paths, nil
}
func QueueExternalOpen(ctx context.Context, address, appID string, paths []string) error {
	payload, _ := json.Marshal(map[string]any{"appId": appID, "paths": paths})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/external-open", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return errors.New("Shell agent rejected external open: " + string(detail))
	}
	return nil
}
