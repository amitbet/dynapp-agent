package shellagent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type sessionRecord struct {
	SessionID string         `json:"sessionId"`
	ProcessID any            `json:"processId"`
	Sequence  int64          `json:"sequence"`
	Status    string         `json:"status"`
	Value     map[string]any `json:"value"`
	CreatedAt int64          `json:"createdAt"`
	UpdatedAt int64          `json:"updatedAt"`
}
type sessionFile struct {
	SchemaVersion int             `json:"schemaVersion"`
	Sessions      []sessionRecord `json:"sessions"`
}

func sessionID(value any) (string, error) {
	id := stringValue(value)
	if !sessionIDPattern.MatchString(id) {
		return "", errors.New("session id is invalid")
	}
	return id, nil
}
func (s *Server) sessionPath(appID string) (string, error) {
	dir, err := s.serviceDir("sessions")
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(appID))
	return filepath.Join(dir, base64.RawURLEncoding.EncodeToString(digest[:])+".json"), nil
}
func (s *Server) readSessions(appID string) (map[string]sessionRecord, error) {
	path, err := s.sessionPath(appID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]sessionRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file sessionFile
	if json.Unmarshal(data, &file) != nil || file.SchemaVersion != 1 {
		return nil, errors.New("session state is invalid")
	}
	result := map[string]sessionRecord{}
	for _, record := range file.Sessions {
		if sessionIDPattern.MatchString(record.SessionID) {
			result[record.SessionID] = record
		}
	}
	return result, nil
}
func (s *Server) writeSessions(appID string, records map[string]sessionRecord) error {
	path, err := s.sessionPath(appID)
	if err != nil {
		return err
	}
	values := make([]sessionRecord, 0, len(records))
	for _, record := range records {
		values = append(values, record)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].SessionID < values[j].SessionID })
	data, _ := json.MarshalIndent(sessionFile{SchemaVersion: 1, Sessions: values}, "", "  ")
	temporary, err := os.CreateTemp(filepath.Dir(path), ".sessions-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(append(data, '\n')); err == nil {
		err = temporary.Close()
	} else {
		_ = temporary.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func (s *Server) handleSessionsRPC(request message) (any, error) {
	if request.AppID == "" {
		return nil, errors.New("app id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readSessions(request.AppID)
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case "list":
		values := make([]sessionRecord, 0, len(records))
		for _, record := range records {
			values = append(values, record)
		}
		sort.Slice(values, func(i, j int) bool { return values[i].UpdatedAt > values[j].UpdatedAt })
		return values, nil
	case "get":
		id, err := sessionID(firstArg(request.Args))
		if err != nil {
			return nil, err
		}
		record, ok := records[id]
		if !ok {
			return nil, nil
		}
		return record, nil
	case "upsert":
		input := objectArg(request.Args, 0)
		id, err := sessionID(input["sessionId"])
		if err != nil {
			return nil, err
		}
		previous := records[id]
		now := time.Now().UnixMilli()
		value, ok := input["value"].(map[string]any)
		if !ok {
			return nil, errors.New("session value must be an object")
		}
		encoded, _ := json.Marshal(value)
		if len(encoded) > 64*1024 {
			return nil, errors.New("session value is limited to 65536 bytes")
		}
		status := stringValue(input["status"])
		if status == "" {
			status = "idle"
		}
		if len(status) > 40 {
			return nil, errors.New("session status is invalid")
		}
		processID := input["processId"]
		if processID == nil {
			processID = previous.ProcessID
		}
		if len(fmt.Sprint(processID)) > 200 {
			return nil, errors.New("session process id is invalid")
		}
		sequence := integer64(input["sequence"])
		if sequence < 0 {
			sequence = previous.Sequence
		}
		created := previous.CreatedAt
		if created == 0 {
			created = now
		}
		record := sessionRecord{SessionID: id, ProcessID: processID, Sequence: sequence, Status: status, Value: value, CreatedAt: created, UpdatedAt: now}
		records[id] = record
		return record, s.writeSessions(request.AppID, records)
	case "remove":
		id, err := sessionID(firstArg(request.Args))
		if err != nil {
			return nil, err
		}
		if _, ok := records[id]; !ok {
			return false, nil
		}
		delete(records, id)
		return true, s.writeSessions(request.AppID, records)
	case "prune":
		cutoff := integer64(firstArg(request.Args))
		if cutoff <= 0 {
			return nil, errors.New("session prune cutoff is invalid")
		}
		removed := []string{}
		for id, record := range records {
			if record.UpdatedAt < cutoff && record.Status != "connecting" && record.Status != "running" {
				removed = append(removed, id)
				delete(records, id)
			}
		}
		sort.Strings(removed)
		if len(removed) > 0 {
			err = s.writeSessions(request.AppID, records)
		}
		return map[string]any{"removed": removed}, err
	default:
		return nil, errors.New("unsupported session method")
	}
}
func firstArg(args []any) any {
	if len(args) > 0 {
		return args[0]
	}
	return nil
}
