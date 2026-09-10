package fs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
)

const (
	MaxReadBytes  = 20 * 1024 * 1024
	MaxWriteBytes = 20 * 1024 * 1024
	MaxChunkBytes = 320 * 1024
)

func Call(method string, args []any, body []byte) (any, error) {
	pathArg := func(index int, label string) (string, error) {
		if len(args) <= index {
			return "", fmt.Errorf("%s is invalid", label)
		}
		value, ok := args[index].(string)
		if !ok || strings.TrimSpace(value) == "" || strings.Contains(value, "\x00") {
			return "", fmt.Errorf("%s is invalid", label)
		}
		return value, nil
	}
	switch method {
	case "home":
		return agentutil.HomeDir(), nil
	case "roots":
		return filesystemRoots()
	case "list":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return listDir(value)
	case "stat":
		value, err := pathArg(0, "Path")
		if err != nil {
			return nil, err
		}
		return statPath(value)
	case "readText":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readText(value, agentutil.NumberArg(args, 1, 2_000_000, MaxReadBytes))
	case "writeText":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		content := agentutil.StringArg(args, 1)
		return nil, os.WriteFile(value, []byte(content), 0o666)
	case "readBase64":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readBase64(value, agentutil.NumberArg(args, 1, MaxReadBytes, MaxReadBytes))
	case "writeBase64":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		raw := body
		if raw == nil {
			decoded, decodeErr := base64.StdEncoding.DecodeString(agentutil.StringArg(args, 1))
			if decodeErr != nil {
				return nil, errors.New("Base64 data is invalid")
			}
			raw = decoded
		}
		if raw == nil {
			return nil, errors.New("Base64 data is invalid")
		}
		if len(raw) > MaxWriteBytes {
			return nil, errors.New("Remote filesystem write is too large")
		}
		err = os.WriteFile(value, raw, 0o666)
		return map[string]any{"path": value, "size": len(raw)}, err
	case "mkdir":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return nil, os.MkdirAll(value, 0o777)
	case "copy":
		source, err := pathArg(0, "Source path")
		if err != nil {
			return nil, err
		}
		target, err := pathArg(1, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, copyPath(source, target)
	case "move":
		source, err := pathArg(0, "Source path")
		if err != nil {
			return nil, err
		}
		target, err := pathArg(1, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, os.Rename(source, target)
	case "remove":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, os.RemoveAll(value)
	case "dirSize":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return dirSize(value)
	case "diskUsage":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return diskUsage(value)
	case "readChunk", "readChunkBinary":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		return readChunk(value, agentutil.NumberArg(args, 1, 0, int(^uint(0)>>1)), agentutil.NumberArg(args, 2, 1_000_000, MaxReadBytes))
	case "writeChunk", "writeChunkBinary":
		value, err := pathArg(0, "File path")
		if err != nil {
			return nil, err
		}
		offset := agentutil.NumberArg(args, 1, 0, int(^uint(0)>>1))
		raw := body
		if raw == nil {
			decoded, decodeErr := base64.StdEncoding.DecodeString(agentutil.StringArg(args, 2))
			if decodeErr != nil {
				return nil, errors.New("Base64 data is invalid")
			}
			raw = decoded
		}
		if raw == nil {
			return nil, errors.New("Base64 data is invalid")
		}
		if len(raw) > MaxChunkBytes {
			return nil, errors.New("Remote filesystem write chunk is too large")
		}
		if err = writeChunk(value, int64(offset), raw, agentutil.BoolArg(args, 3)); err != nil {
			return nil, err
		}
		return map[string]any{"path": value, "offset": offset, "bytesWritten": len(raw)}, nil
	case "trash":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, trashPath(value)
	case "openPath":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, openDesktopPath(value)
	case "openWithOptions":
		return desktopOpenWithOptions(), nil
	case "openWith":
		value, err := pathArg(0, "Target path")
		if err != nil {
			return nil, err
		}
		return nil, openDesktopPathWith(value, agentutil.StringArg(args, 1))
	case "execTerminal":
		return nil, openTerminal(agentutil.StringArg(args, 0), agentutil.StringArg(args, 1))
	case "packZip":
		if len(args) < 2 {
			return nil, errors.New("archive request is invalid")
		}
		var sources []string
		if raw, ok := args[0].([]any); ok {
			for _, item := range raw {
				if value := agentutil.StringValue(item); value != "" {
					sources = append(sources, value)
				}
			}
		}
		target, err := pathArg(1, "Archive path")
		if err != nil {
			return nil, err
		}
		return nil, createZip(sources, target, agentutil.ObjectArg(args, 2))
	case "watchSnapshot":
		value, err := pathArg(0, "Directory path")
		if err != nil {
			return nil, err
		}
		return watchSnapshot(value)
	default:
		return nil, errors.New("Remote filesystem method is not supported")
	}
}
