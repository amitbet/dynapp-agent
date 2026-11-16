package serverkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyFileName is the instance key file created in Config.DataDir.
const KeyFileName = "dynapp-server-key.pem"

// JWK is a public P-256 key in JSON Web Key form.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

var b64 = base64.RawURLEncoding

// loadOrCreateKey reads the PEM instance key at path, or creates one (0600)
// when the file does not exist. A present but unreadable key is an error: the
// key is the server's identity and is never silently replaced.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return parseKey(raw)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("serverkit: reading instance key: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("serverkit: creating data directory: %w", err)
	}
	// O_EXCL: two processes starting at once must not each write a key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if raw, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("serverkit: reading instance key: %w", err)
		}
		return parseKey(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("serverkit: creating instance key: %w", err)
	}
	if err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("serverkit: writing instance key: %w", err)
	}
	return key, nil
}

func parseKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("serverkit: instance key file is not PEM")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("serverkit: unexpected PEM block %q in instance key file", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("serverkit: parsing instance key: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("serverkit: instance key is not a P-256 ECDSA key")
	}
	return ec, nil
}

// PublicJWK returns the JWK form of a P-256 public key.
func PublicJWK(pub *ecdsa.PublicKey) JWK {
	x, y := make([]byte, 32), make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	return JWK{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(x), Y: b64.EncodeToString(y)}
}

// Thumbprint is the RFC 7638 thumbprint of an EC JWK: base64url SHA-256 of
// the canonical JSON {"crv","kty","x","y"} with no whitespace.
func Thumbprint(k JWK) string {
	canonical := `{"crv":` + quote(k.Crv) + `,"kty":` + quote(k.Kty) + `,"x":` + quote(k.X) + `,"y":` + quote(k.Y) + `}`
	sum := sha256.Sum256([]byte(canonical))
	return b64.EncodeToString(sum[:])
}

// quote writes a JSON string. JWK members are base64url or short ASCII names,
// so only the characters JSON requires are escaped.
func quote(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			out = append(out, '\\', c)
		case c < 0x20:
			out = append(out, fmt.Sprintf(`\u%04x`, c)...)
		default:
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}
