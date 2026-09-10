package shellagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshKnownHosts struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Hosts         map[string]sshKnownHost `json:"hosts"`
}

type sshKnownHost struct {
	Fingerprint string `json:"fingerprint"`
	SeenAt      string `json:"seenAt"`
}

func (s *Server) verifySSHHostKey(host string, port int, key ssh.PublicKey) error {
	hostname := strings.ToLower(strings.TrimSpace(host))
	seen := strings.ToLower(strings.TrimSpace(ssh.FingerprintSHA256(key)))
	if hostname == "" || seen == "" {
		return errors.New("SSH host fingerprint is required")
	}
	if port <= 0 {
		port = 22
	}
	id := hostname + ":" + strconv.Itoa(port)
	root, err := s.serviceDir("ssh")
	if err != nil {
		return err
	}
	path := filepath.Join(root, "known-hosts.json")
	hosts := map[string]sshKnownHost{}
	if data, readErr := os.ReadFile(path); readErr == nil {
		var parsed sshKnownHosts
		if json.Unmarshal(data, &parsed) == nil && parsed.SchemaVersion == 1 && parsed.Hosts != nil {
			hosts = parsed.Hosts
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	previous, ok := hosts[id]
	if !ok || strings.TrimSpace(previous.Fingerprint) == "" {
		hosts[id] = sshKnownHost{Fingerprint: seen, SeenAt: time.Now().UTC().Format(time.RFC3339)}
		payload, err := json.MarshalIndent(sshKnownHosts{SchemaVersion: 1, Hosts: hosts}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		temporary := path + ".tmp"
		if err := os.WriteFile(temporary, append(payload, '\n'), 0o600); err != nil {
			return err
		}
		return os.Rename(temporary, path)
	}
	if previous.Fingerprint != seen {
		return errors.New("SSH host key does not match the pinned fingerprint")
	}
	return nil
}
