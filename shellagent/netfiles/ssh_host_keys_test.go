package netfiles

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

type staticPublicKey struct{ blob []byte }

func (key staticPublicKey) Type() string    { return "ssh-ed25519" }
func (key staticPublicKey) Marshal() []byte { return key.blob }
func (key staticPublicKey) Verify([]byte, *ssh.Signature) error {
	return nil
}

func TestSSHHostKeysTrustOnFirstUse(t *testing.T) {
	stateDir := t.TempDir()
	first := staticPublicKey{blob: []byte("host-key-one")}
	second := staticPublicKey{blob: []byte("host-key-two")}
	if err := VerifyHostKey(stateDir, "sftp.example.test", 22, first); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHostKey(stateDir, "sftp.example.test", 22, first); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHostKey(stateDir, "sftp.example.test", 22, second); err == nil {
		t.Fatal("changed host key was accepted")
	}
	if err := VerifyHostKey(stateDir, "sftp.example.test", 2222, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ssh", "known-hosts.json")); err != nil {
		t.Fatal(err)
	}
}
