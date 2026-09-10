package netfiles

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
	ftpclient "github.com/jlaffaye/ftp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type networkFileClient interface {
	List(string) (any, error)
	Stat(string) (any, error)
	Read(string, int64) ([]byte, int64, error)
	OpenRead(string) (io.ReadCloser, int64, error)
	OpenReadAt(string, int64) (io.ReadCloser, int64, error)
	OpenWrite(string) (io.WriteCloser, error)
	Write(string, []byte) error
	Mkdir(string) error
	Remove(string) error
	Rename(string, string) error
	Close() error
}

func ftpControlArgument(value any, label string) (string, error) {
	text := agentutil.StringValue(value)
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("Invalid FTP %s", label)
	}
	return text, nil
}

func joinLocalCopyPath(rootPath, entryName string) (string, error) {
	if strings.ContainsRune(entryName, 0) {
		return "", errors.New("Invalid remote listing name")
	}
	resolvedRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	relative := []string{}
	for _, part := range strings.Split(strings.ReplaceAll(entryName, "\\", "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(relative) > 0 {
				relative = relative[:len(relative)-1]
			}
			continue
		}
		if len(part) == 2 && part[1] == ':' && ((part[0] >= 'A' && part[0] <= 'Z') || (part[0] >= 'a' && part[0] <= 'z')) {
			return "", errors.New("Remote listing escaped the destination directory")
		}
		if strings.ContainsAny(part, "\r\n") {
			return "", errors.New("Invalid FTP path")
		}
		relative = append(relative, part)
	}
	joined := resolvedRoot
	if len(relative) > 0 {
		joined = filepath.Join(append([]string{resolvedRoot}, relative...)...)
	}
	contained, err := filepath.Rel(resolvedRoot, joined)
	if err != nil || strings.HasPrefix(contained, "..") || filepath.IsAbs(contained) {
		return "", errors.New("Remote listing escaped the destination directory")
	}
	return joined, nil
}

func remotePath(value any) (string, error) {
	parts := strings.FieldsFunc(strings.ReplaceAll(agentutil.StringValue(value), "\\", "/"), func(r rune) bool { return r == '/' })
	clean := []string{}
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if strings.ContainsAny(part, "\r\n") {
			return "", errors.New("Invalid FTP path")
		}
		if part == ".." {
			if len(clean) > 0 {
				clean = clean[:len(clean)-1]
			}
			continue
		}
		clean = append(clean, part)
	}
	return "/" + strings.Join(clean, "/"), nil
}
func connectionValues(value any) (host, user, password, identity string, port int, err error) {
	record, _ := value.(map[string]any)
	host, err = ftpControlArgument(strings.TrimSpace(agentutil.StringValue(record["host"])), "host")
	if err != nil {
		return
	}
	user, err = ftpControlArgument(record["user"], "username")
	if err != nil {
		return
	}
	password, err = ftpControlArgument(record["password"], "password")
	if err != nil {
		return
	}
	identity, err = ftpControlArgument(record["identityFile"], "identity")
	if err != nil {
		return
	}
	if number, ok := record["port"].(float64); ok {
		port = int(number)
	}
	return
}
func HandleRPC(ctx context.Context, stateDir, kind, method string, args []any, publish func(map[string]any)) (any, error) {
	if len(args) < 1 {
		return nil, errors.New("network connection is required")
	}
	client, err := Open(ctx, stateDir, kind, args[0])
	if err != nil {
		return nil, err
	}
	defer client.Close()
	pathArg := func(index int) (string, error) {
		if index >= len(args) {
			return "", nil
		}
		return remotePath(args[index])
	}
	sourcePath, err := pathArg(1)
	if err != nil {
		return nil, err
	}
	secondPath, err := pathArg(2)
	if err != nil {
		return nil, err
	}
	progressID := ""
	if len(args) > 3 {
		progressID = agentutil.StringValue(args[len(args)-1])
	}
	progress := func(event map[string]any) {
		if progressID == "" || publish == nil {
			return
		}
		event["progressId"] = progressID
		publish(event)
	}
	switch method {
	case "list":
		return client.List(sourcePath)
	case "stat":
		return client.Stat(sourcePath)
	case "readText", "readBase64":
		limit := int64(20 * 1024 * 1024)
		if len(args) > 2 {
			if value, ok := args[2].(float64); ok && value > 0 {
				limit = int64(value)
			}
		}
		data, size, err := client.Read(sourcePath, limit)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"size": size, "truncated": int64(len(data)) < size}
		if method == "readText" {
			result["content"] = string(data)
		} else {
			result["base64"] = base64.StdEncoding.EncodeToString(data)
		}
		return result, nil
	case "readChunk":
		offset, length := int64(0), int64(0)
		if value, ok := args[2].(float64); ok {
			offset = int64(value)
		}
		if value, ok := args[3].(float64); ok {
			length = int64(value)
		}
		if offset < 0 || length < 0 {
			return nil, errors.New("chunk offset and length must be non-negative")
		}
		reader, size, err := client.OpenReadAt(sourcePath, offset)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, length))
		if err != nil {
			return nil, err
		}
		end := offset + int64(len(data))
		return map[string]any{"base64": base64.StdEncoding.EncodeToString(data), "size": size, "eof": end >= size, "truncated": false}, nil
	case "writeText":
		return true, client.Write(sourcePath, []byte(agentutil.StringValue(args[2])))
	case "mkdir":
		return true, client.Mkdir(sourcePath)
	case "remove":
		return true, client.Remove(sourcePath)
	case "rename":
		return true, client.Rename(sourcePath, secondPath)
	case "copyRemote":
		total, _ := networkPathSize(client, sourcePath)
		return true, copyNetworkRemote(client, sourcePath, secondPath, &copyProgress{total: total, publish: progress})
	case "copyToLocal":
		total, _ := networkPathSize(client, sourcePath)
		return true, copyNetworkToLocal(client, sourcePath, agentutil.StringValue(args[2]), &copyProgress{total: total, publish: progress})
	case "copyFromLocal":
		total, _ := localPathSize(agentutil.StringValue(args[1]))
		return true, copyLocalToNetwork(client, agentutil.StringValue(args[1]), secondPath, &copyProgress{total: total, publish: progress})
	case "dirSize":
		return networkDirSize(client, sourcePath)
	default:
		return nil, errors.New("unsupported network filesystem method")
	}
}
func Open(ctx context.Context, stateDir, kind string, value any) (networkFileClient, error) {
	host, user, password, identity, port, err := connectionValues(value)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, errors.New("network host is required")
	}
	if kind == "ftp" {
		if port == 0 {
			port = 21
		}
		connection, err := ftpclient.Dial(net.JoinHostPort(host, fmt.Sprint(port)), ftpclient.DialWithContext(ctx), ftpclient.DialWithTimeout(20*time.Second))
		if err != nil {
			return nil, err
		}
		if user == "" {
			user = "anonymous"
		}
		if password == "" {
			password = "anonymous@"
		}
		if err := connection.Login(user, password); err != nil {
			connection.Quit()
			return nil, err
		}
		return &ftpFiles{connection}, nil
	}
	if port == 0 {
		port = 22
	}
	auth := []ssh.AuthMethod{}
	if password != "" {
		auth = append(auth, ssh.Password(password))
	}
	if identity != "" {
		data, err := os.ReadFile(identity)
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	sshConnection, err := ssh.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)), &ssh.ClientConfig{
		User: user,
		Auth: auth,
		HostKeyCallback: func(hostname string, _ net.Addr, key ssh.PublicKey) error {
			return VerifyHostKey(stateDir, host, port, key)
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	client, err := sftp.NewClient(sshConnection)
	if err != nil {
		sshConnection.Close()
		return nil, err
	}
	return &sftpFiles{client: client, ssh: sshConnection}, nil
}
func entryValue(name string, isDir, isLink bool, size int64, mtime time.Time, mode os.FileMode) map[string]any {
	return map[string]any{"name": name, "isDir": isDir, "isSymlink": isLink, "size": size, "mtimeMs": mtime.UnixMilli(), "mode": uint32(mode)}
}

type ftpFiles struct{ *ftpclient.ServerConn }

func (f *ftpFiles) List(value string) (any, error) {
	entries, err := f.ServerConn.List(value)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		out = append(out, entryValue(entry.Name, entry.Type == ftpclient.EntryTypeFolder, entry.Type == ftpclient.EntryTypeLink, int64(entry.Size), entry.Time, 0))
	}
	return map[string]any{"path": value, "entries": out}, nil
}
func (f *ftpFiles) Stat(value string) (any, error) {
	entry, err := f.GetEntry(value)
	if err != nil {
		return nil, nil
	}
	return entryValue(entry.Name, entry.Type == ftpclient.EntryTypeFolder, entry.Type == ftpclient.EntryTypeLink, int64(entry.Size), entry.Time, 0), nil
}
func (f *ftpFiles) Read(value string, limit int64) ([]byte, int64, error) {
	response, size, err := f.OpenRead(value)
	if err != nil {
		return nil, 0, err
	}
	defer response.Close()
	reader := io.LimitReader(response, limit+1)
	data, err := io.ReadAll(reader)
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, size, err
}
func (f *ftpFiles) OpenRead(value string) (io.ReadCloser, int64, error) {
	entry, err := f.GetEntry(value)
	if err != nil {
		return nil, 0, err
	}
	response, err := f.Retr(value)
	return response, int64(entry.Size), err
}
func (f *ftpFiles) OpenReadAt(value string, offset int64) (io.ReadCloser, int64, error) {
	entry, err := f.GetEntry(value)
	if err != nil {
		return nil, 0, err
	}
	if offset > int64(entry.Size) {
		offset = int64(entry.Size)
	}
	response, err := f.RetrFrom(value, uint64(offset))
	return response, int64(entry.Size), err
}

type ftpWriteCloser struct {
	writer *io.PipeWriter
	done   chan error
}

func (writer *ftpWriteCloser) Write(data []byte) (int, error) { return writer.writer.Write(data) }
func (writer *ftpWriteCloser) Close() error {
	err := writer.writer.Close()
	result := <-writer.done
	if err != nil {
		return err
	}
	return result
}
func (f *ftpFiles) OpenWrite(value string) (io.WriteCloser, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- f.Stor(value, reader); _ = reader.Close() }()
	return &ftpWriteCloser{writer: writer, done: done}, nil
}
func (f *ftpFiles) Write(value string, data []byte) error {
	return f.Stor(value, bytes.NewReader(data))
}
func (f *ftpFiles) Mkdir(value string) error {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	current := ""
	for _, part := range parts {
		current += "/" + part
		if err := f.MakeDir(current); err != nil {
			if entry, _ := f.GetEntry(current); entry == nil || entry.Type != ftpclient.EntryTypeFolder {
				return err
			}
		}
	}
	return nil
}
func (f *ftpFiles) Remove(value string) error {
	entry, err := f.GetEntry(value)
	if err != nil {
		return nil
	}
	if entry.Type == ftpclient.EntryTypeFolder {
		return f.RemoveDirRecur(value)
	}
	return f.Delete(value)
}
func (f *ftpFiles) Close() error { return f.Quit() }

type sftpFiles struct {
	client *sftp.Client
	ssh    *ssh.Client
}

func (f *sftpFiles) List(value string) (any, error) {
	entries, err := f.client.ReadDir(value)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, entry := range entries {
		out = append(out, entryValue(entry.Name(), entry.IsDir(), entry.Mode()&os.ModeSymlink != 0, entry.Size(), entry.ModTime(), entry.Mode()))
	}
	return map[string]any{"path": value, "entries": out}, nil
}
func (f *sftpFiles) Stat(value string) (any, error) {
	info, err := f.client.Stat(value)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entryValue(path.Base(value), info.IsDir(), info.Mode()&os.ModeSymlink != 0, info.Size(), info.ModTime(), info.Mode()), nil
}
func (f *sftpFiles) Read(value string, limit int64) ([]byte, int64, error) {
	file, size, err := f.OpenRead(value)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit))
	return data, size, err
}
func (f *sftpFiles) OpenRead(value string) (io.ReadCloser, int64, error) {
	file, err := f.client.Open(value)
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
func (f *sftpFiles) OpenReadAt(value string, offset int64) (io.ReadCloser, int64, error) {
	file, size, err := f.OpenRead(value)
	if err != nil {
		return nil, 0, err
	}
	if offset > size {
		offset = size
	}
	if _, err := file.(*sftp.File).Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, size, nil
}
func (f *sftpFiles) OpenWrite(value string) (io.WriteCloser, error) { return f.client.Create(value) }
func (f *sftpFiles) Write(value string, data []byte) error {
	file, err := f.client.Create(value)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}
func (f *sftpFiles) Mkdir(value string) error { return f.client.MkdirAll(value) }
func (f *sftpFiles) Remove(value string) error {
	info, err := f.client.Stat(value)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return f.client.Remove(value)
	}
	entries, err := f.client.ReadDir(value)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := f.Remove(path.Join(value, entry.Name())); err != nil {
			return err
		}
	}
	return f.client.RemoveDirectory(value)
}
func (f *sftpFiles) Rename(from, to string) error { return f.client.Rename(from, to) }
func (f *sftpFiles) Close() error                 { _ = f.client.Close(); return f.ssh.Close() }
func networkDirSize(client networkFileClient, root string) (map[string]any, error) {
	bytes, files, dirs := int64(0), 0, 0
	var walk func(string) error
	walk = func(dir string) error {
		listing, err := client.List(dir)
		if err != nil {
			return err
		}
		record, _ := listing.(map[string]any)
		entries, _ := record["entries"].([]map[string]any)
		for _, entry := range entries {
			child := path.Join(dir, agentutil.StringValue(entry["name"]))
			if isDir, _ := entry["isDir"].(bool); isDir {
				dirs++
				if err := walk(child); err != nil {
					return err
				}
			} else {
				files++
				if size, ok := entry["size"].(int64); ok {
					bytes += size
				} else if size, ok := entry["size"].(uint64); ok {
					bytes += int64(size)
				}
			}
		}
		return nil
	}
	err := walk(root)
	return map[string]any{"bytes": bytes, "files": files, "dirs": dirs, "truncated": false}, err
}

type copyProgress struct {
	bytes, total int64
	publish      func(map[string]any)
}

func (progress *copyProgress) add(phase, value string, count int64) {
	progress.bytes += count
	progress.publish(map[string]any{"phase": phase, "path": value, "bytes": progress.bytes, "total": progress.total})
}
func networkPathSize(client networkFileClient, value string) (int64, error) {
	stat, err := client.Stat(value)
	if err != nil || stat == nil {
		return 0, err
	}
	record := stat.(map[string]any)
	if record["isDir"] == true {
		result, err := networkDirSize(client, value)
		if err != nil {
			return 0, err
		}
		return integer64(result["bytes"]), nil
	}
	return integer64(record["size"]), nil
}
func integer64(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case uint64:
		return int64(number)
	case float64:
		return int64(number)
	}
	return 0
}
func streamCopy(reader io.Reader, writer io.Writer, phase, value string, progress *copyProgress) error {
	buffer := make([]byte, 256*1024)
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			written, writeErr := writer.Write(buffer[:count])
			if writeErr != nil {
				return writeErr
			}
			progress.add(phase, value, int64(written))
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
func copyNetworkRemote(client networkFileClient, source, target string, progress *copyProgress) error {
	stat, err := client.Stat(source)
	if err != nil {
		return err
	}
	if stat == nil {
		return os.ErrNotExist
	}
	if stat.(map[string]any)["isDir"] == true {
		if err := client.Mkdir(target); err != nil {
			return err
		}
		listing, err := client.List(source)
		if err != nil {
			return err
		}
		for _, entry := range listing.(map[string]any)["entries"].([]map[string]any) {
			name := agentutil.StringValue(entry["name"])
			if err := copyNetworkRemote(client, path.Join(source, name), path.Join(target, name), progress); err != nil {
				return err
			}
		}
		return nil
	}
	reader, _, err := client.OpenRead(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := client.OpenWrite(target)
	if err != nil {
		return err
	}
	if err := streamCopy(reader, writer, "upload", source, progress); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}
func copyNetworkToLocal(client networkFileClient, source, target string, progress *copyProgress) error {
	stat, err := client.Stat(source)
	if err != nil {
		return err
	}
	if stat == nil {
		return os.ErrNotExist
	}
	if stat.(map[string]any)["isDir"] == true {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		listing, err := client.List(source)
		if err != nil {
			return err
		}
		for _, entry := range listing.(map[string]any)["entries"].([]map[string]any) {
			name := agentutil.StringValue(entry["name"])
			child, joinErr := joinLocalCopyPath(target, name)
			if joinErr != nil {
				return joinErr
			}
			if err := copyNetworkToLocal(client, path.Join(source, name), child, progress); err != nil {
				return err
			}
		}
		return nil
	}
	reader, _, err := client.OpenRead(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	writer, err := os.CreateTemp(filepath.Dir(target), ".dynapp-download-*")
	if err != nil {
		return err
	}
	temporary := writer.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if err := streamCopy(reader, writer, "download", source, progress); err != nil {
		writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	committed = true
	return nil
}
func localPathSize(value string) (int64, error) {
	stat, err := os.Stat(value)
	if err != nil {
		return 0, err
	}
	if !stat.IsDir() {
		return stat.Size(), nil
	}
	var total int64
	err = filepath.Walk(value, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return err
	})
	return total, err
}
func copyLocalToNetwork(client networkFileClient, source, target string, progress *copyProgress) error {
	stat, err := os.Stat(source)
	if err != nil {
		return err
	}
	if stat.IsDir() {
		if err := client.Mkdir(target); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyLocalToNetwork(client, filepath.Join(source, entry.Name()), path.Join(target, entry.Name()), progress); err != nil {
				return err
			}
		}
		return nil
	}
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := client.OpenWrite(target)
	if err != nil {
		return err
	}
	if err := streamCopy(reader, writer, "upload", source, progress); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}
