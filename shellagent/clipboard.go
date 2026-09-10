package shellagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const maxClipboardFiles = 32

func (s *Server) handleClipboardRPC(request message) (any, error) {
	return s.handleClipboardRPCWithSocket(nil, request, nil)
}

func (s *Server) handleClipboardRPCWithSocket(socket protocolSocket, request message, body []byte) (any, error) {
	switch request.Method {
	case "allocate":
		return s.allocateClipboardFile(request)
	case "write":
		return s.writeClipboardFileChunk(request)
	case "publish":
		return s.publishClipboardFiles(request)
	case "writeText":
		return s.writeClipboardText(request)
	case "publishPromises":
		return s.publishClipboardPromises(socket, request)
	case "listPromises":
		return s.listClipboardPromises(), nil
	case "requestPromise":
		return s.requestClipboardPromise(socket, request)
	case "writePromise":
		return s.writeClipboardPromise(request)
	case "writePromiseBinary":
		return s.writeClipboardPromiseBinary(request, body)
	case "promiseStatus":
		return s.clipboardPromiseStatus(request)
	case "finishPromise":
		return s.finishClipboardPromise(request)
	case "cancelPromises":
		return s.cancelClipboardPromises(request)
	default:
		return nil, errors.New("unsupported clipboard method")
	}
}

func (s *Server) writeClipboardPromiseBinary(request message, raw []byte) (any, error) {
	promise, err := s.ownedPromise(request)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 4*1024*1024 {
		return nil, errors.New("clipboard promise binary chunk is invalid")
	}
	offset := int64(numberArg(request.Args, 1, 0, 1<<53-1))
	if _, err = promise.file.WriteAt(raw, offset); err != nil {
		return nil, err
	}
	return map[string]any{"bytesWritten": len(raw)}, nil
}

type clipboardPromise struct {
	id                string
	appID             string
	sourceID          string
	name              string
	size              int64
	socket            protocolSocket
	destinationSocket protocolSocket
	destinationAppID  string
	path              string
	file              *os.File
}

type promisedFileRequest struct {
	SourceID string `json:"sourceId"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
}

func clipboardPromiseID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return "promise_" + hex.EncodeToString(value)
}

func (s *Server) publishClipboardPromises(socket protocolSocket, request message) (any, error) {
	raw, ok := request.Args[0].([]any)
	if !ok || len(raw) == 0 || len(raw) > maxClipboardFiles {
		return nil, errors.New("clipboard promises require between 1 and 32 files")
	}
	entries := make([]filePromiseDescriptor, 0, len(raw))
	s.promiseMu.Lock()
	if s.promises == nil {
		s.promises = make(map[string]*clipboardPromise)
	}
	for _, item := range raw {
		value, ok := item.(map[string]any)
		if !ok {
			s.promiseMu.Unlock()
			return nil, errors.New("clipboard promise metadata is invalid")
		}
		name := filepath.Base(strings.ReplaceAll(stringValue(value["name"]), "\x00", ""))
		sourceID := stringValue(value["sourceId"])
		size := int64(numberArg([]any{value["size"]}, 0, 0, 1<<53-1))
		if name == "" || name == "." || sourceID == "" {
			s.promiseMu.Unlock()
			return nil, errors.New("clipboard promise metadata is invalid")
		}
		id := clipboardPromiseID()
		s.promises[id] = &clipboardPromise{id: id, appID: request.AppID, sourceID: sourceID, name: name, size: size, socket: socket}
		entries = append(entries, filePromiseDescriptor{ID: id, Name: name, Size: size})
	}
	s.promiseMu.Unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var bridge *filePromiseBridge
		bridge, err = s.ensureFilePromiseBridge()
		if err != nil {
			break
		}
		bridge.onRequest = s.handleFilePromiseRequest
		if err = bridge.publish(entries); err == nil {
			return map[string]any{"count": len(entries)}, nil
		}
		s.discardFilePromiseBridge(bridge)
	}
	return nil, err
}

func (s *Server) handleFilePromiseRequest(id, path string) {
	_ = s.requestFilePromise(id, path, nil, "")
}

func (s *Server) requestFilePromise(id, path string, destinationSocket protocolSocket, destinationAppID string) error {
	s.promiseMu.Lock()
	promise := s.promises[id]
	if promise == nil {
		s.promiseMu.Unlock()
		return errors.New("clipboard promise is not active")
	}
	if promise.file != nil {
		s.promiseMu.Unlock()
		return errors.New("clipboard promise is already being pasted")
	}
	promise.path = path
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		promise.file = file
		promise.destinationSocket = destinationSocket
		promise.destinationAppID = destinationAppID
	}
	sourceSocket := promise.socket
	sourceID, name, size := promise.sourceID, promise.name, promise.size
	s.promiseMu.Unlock()
	if err != nil {
		s.promiseMu.Lock()
		bridge := s.promiseBridge
		s.promiseMu.Unlock()
		if bridge != nil {
			_ = bridge.complete(id, err)
		}
		return err
	}
	if sourceSocket != nil {
		send(sourceSocket, context.Background(), map[string]any{"type": "rpc-event", "service": "clipboard", "event": map[string]any{
			"type": "file-promise-request", "promiseId": id, "sourceId": sourceID, "name": name, "size": size,
		}})
	}
	return nil
}

func (s *Server) listClipboardPromises() []map[string]any {
	s.promiseMu.Lock()
	defer s.promiseMu.Unlock()
	items := make([]map[string]any, 0, len(s.promises))
	for id, promise := range s.promises {
		if promise == nil || promise.file != nil {
			continue
		}
		items = append(items, map[string]any{
			"promiseId": id,
			"sourceId":  promise.sourceID,
			"name":      promise.name,
			"size":      promise.size,
		})
	}
	return items
}

func clipboardAvailablePath(directory, name string) (string, error) {
	if directory == "" || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", errors.New("clipboard destination is invalid")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("clipboard destination is not a directory")
	}
	candidate := filepath.Join(directory, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}
	stem, extension := strings.TrimSuffix(name, filepath.Ext(name)), filepath.Ext(name)
	for index := 2; index < 10000; index++ {
		copyName := fmt.Sprintf("%s %d%s", stem, index, extension)
		candidate = filepath.Join(directory, copyName)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", errors.New("could not choose a free clipboard destination")
}

func (s *Server) requestClipboardPromise(socket protocolSocket, request message) (any, error) {
	id := stringArg(request.Args, 0)
	directory := stringArg(request.Args, 1)
	s.promiseMu.Lock()
	promise := s.promises[id]
	if promise == nil || promise.file != nil {
		s.promiseMu.Unlock()
		return nil, errors.New("clipboard promise is not active")
	}
	name, sourceID, size := promise.name, promise.sourceID, promise.size
	s.promiseMu.Unlock()
	path, err := clipboardAvailablePath(directory, name)
	if err != nil {
		return nil, err
	}
	if err = s.requestFilePromise(id, path, socket, request.AppID); err != nil {
		return nil, err
	}
	return map[string]any{"promiseId": id, "sourceId": sourceID, "name": name, "size": size, "path": path}, nil
}

func (s *Server) ownedPromise(request message) (*clipboardPromise, error) {
	id := stringArg(request.Args, 0)
	s.promiseMu.Lock()
	defer s.promiseMu.Unlock()
	promise := s.promises[id]
	if promise == nil || promise.appID != request.AppID || promise.file == nil {
		return nil, errors.New("clipboard promise is not active")
	}
	return promise, nil
}

func (s *Server) writeClipboardPromise(request message) (any, error) {
	promise, err := s.ownedPromise(request)
	if err != nil {
		return nil, err
	}
	offset := int64(numberArg(request.Args, 1, 0, 1<<53-1))
	raw, err := base64.StdEncoding.DecodeString(stringArg(request.Args, 2))
	if err != nil || len(raw) > 240*1024 {
		return nil, errors.New("clipboard promise chunk is invalid")
	}
	if _, err = promise.file.WriteAt(raw, offset); err != nil {
		return nil, err
	}
	return map[string]any{"bytesWritten": len(raw)}, nil
}

func (s *Server) clipboardPromiseStatus(request message) (any, error) {
	promise, err := s.ownedPromise(request)
	if err != nil {
		return nil, err
	}
	info, err := promise.file.Stat()
	if err != nil {
		return nil, err
	}
	return map[string]any{"bytesWritten": info.Size(), "size": promise.size, "path": promise.path}, nil
}

func (s *Server) finishClipboardPromise(request message) (any, error) {
	promise, err := s.ownedPromise(request)
	if err != nil {
		return nil, err
	}
	if err = promise.file.Sync(); err == nil {
		err = promise.file.Close()
	}
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(promise.path)
		if err == nil && info.Size() != promise.size {
			err = errors.New("clipboard promise size does not match")
		}
	}
	s.promiseMu.Lock()
	destinationSocket := promise.destinationSocket
	destinationAppID := promise.destinationAppID
	bridge := s.promiseBridge
	delete(s.promises, promise.id)
	s.promiseMu.Unlock()
	if bridge != nil {
		_ = bridge.complete(promise.id, err)
	}
	if destinationSocket != nil {
		send(destinationSocket, context.Background(), map[string]any{"type": "rpc-event", "service": "clipboard", "event": map[string]any{
			"type": "file-promise-complete", "promiseId": promise.id, "path": promise.path, "name": promise.name, "size": promise.size, "appId": destinationAppID,
			"error": errorString(err),
		}})
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": promise.path, "size": promise.size}, nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) cancelClipboardPromises(request message) (any, error) {
	s.promiseMu.Lock()
	ids := make([]string, 0)
	for id, promise := range s.promises {
		if promise.appID == request.AppID {
			if promise.file != nil {
				_ = promise.file.Close()
				_ = os.Remove(promise.path)
			}
			delete(s.promises, id)
			ids = append(ids, id)
		}
	}
	s.promiseMu.Unlock()
	for _, id := range ids {
		_ = s.promiseBridge.complete(id, errors.New("clipboard promise cancelled"))
	}
	return map[string]any{"count": len(ids)}, nil
}

func (s *Server) clipboardDirectory(appID string) (string, error) {
	root := s.StateDir
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return "", err
		}
	}
	name := safeName(appID)
	if name == "" {
		name = "app"
	}
	directory := filepath.Join(root, "clipboard-files", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	return directory, nil
}

func (s *Server) ownedClipboardPath(appID, path string) (string, error) {
	directory, err := s.clipboardDirectory(appID)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(directory, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("clipboard file path is outside app state")
	}
	return absolute, nil
}

func (s *Server) allocateClipboardFile(request message) (any, error) {
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
		return nil, errors.New("clipboard file metadata is invalid")
	}
	directory, err := s.clipboardDirectory(request.AppID)
	if err != nil {
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

func (s *Server) writeClipboardFileChunk(request message) (any, error) {
	path, err := s.ownedClipboardPath(request.AppID, stringArg(request.Args, 0))
	if err != nil {
		return nil, err
	}
	offset := numberArg(request.Args, 1, 0, maxImportedFileBytes)
	raw, err := base64.StdEncoding.DecodeString(stringArg(request.Args, 2))
	if err != nil || len(raw) > 240*1024 {
		return nil, errors.New("clipboard file chunk is invalid")
	}
	if offset+len(raw) > maxImportedFileBytes {
		return nil, errors.New("clipboard file exceeds the size limit")
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	written, err := file.WriteAt(raw, int64(offset))
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "offset": offset, "bytesWritten": written}, nil
}

func (s *Server) publishClipboardFiles(request message) (any, error) {
	values := sliceArg(request.Args, 0)
	if len(values) == 0 || len(values) > maxClipboardFiles {
		return nil, errors.New("clipboard.writeFiles requires between 1 and 32 files")
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		path, err := s.ownedClipboardPath(request.AppID, stringValue(value))
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := publishNativeClipboardFiles(paths); err != nil {
		return nil, err
	}
	return map[string]any{"count": len(paths)}, nil
}

func appleScriptString(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

func macClipboardScript(paths []string) string {
	if len(paths) == 1 {
		return "set the clipboard to POSIX file " + appleScriptString(paths[0])
	}
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, "POSIX file "+appleScriptString(path))
	}
	return "set the clipboard to {" + strings.Join(parts, ", ") + "}"
}

func publishNativeClipboardFiles(paths []string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("osascript", "-e", macClipboardScript(paths)).Run()
	case "windows":
		quoted := make([]string, 0, len(paths))
		for _, path := range paths {
			quoted = append(quoted, "'"+strings.ReplaceAll(path, "'", "''")+"'")
		}
		return exec.Command("powershell.exe", "-NoProfile", "-Command", "Set-Clipboard -Path @("+strings.Join(quoted, ",")+")").Run()
	default:
		return errors.New("file clipboard is not supported on this platform")
	}
}

func (s *Server) writeClipboardText(request message) (any, error) {
	text := stringArg(request.Args, 0)
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("pbcopy")
		command.Stdin = strings.NewReader(text)
	case "windows":
		command = exec.Command("powershell.exe", "-NoProfile", "-Command", "Set-Clipboard -Value $input")
		command.Stdin = strings.NewReader(text)
	default:
		command = exec.Command("xclip", "-selection", "clipboard")
		command.Stdin = strings.NewReader(text)
	}
	if err := command.Run(); err != nil {
		return nil, err
	}
	return map[string]any{"bytes": len(text)}, nil
}
