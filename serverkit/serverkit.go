// Package serverkit makes a Go HTTP server DynApp-aware (Phase 3a of the
// DynApp mesh network): it owns the server's instance key, serves the
// descriptor and challenge endpoints, announces the server to the local
// DynApp agent and verifies server-scoped DynApp identity tokens.
//
// It depends on the standard library only.
package serverkit

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// DescriptorPath serves the server descriptor (GET, no auth).
	DescriptorPath = "/.well-known/dynapp-server"
	// ChallengePath answers proof-of-key challenges (POST, no auth).
	ChallengePath = "/.well-known/dynapp-server/challenge"
	// DefaultIssuer is the Dyner origin whose tokens are accepted by default.
	DefaultIssuer = "https://dynapp.io"
	// DefaultAgentURL is the local DynApp agent that receives announcements.
	DefaultAgentURL = "http://127.0.0.1:9011"

	challengePrefix = "dynapp-server-challenge/v1\n"
)

// API names an API the server provides: an API id "<owner>/<name>" and its
// integer major version.
type API struct {
	API string `json:"api"`
	V   int    `json:"v"`
}

// Config describes the server. DataDir, Name, Version and Provides are
// required.
type Config struct {
	// DataDir holds the instance key (KeyFileName, created on first run).
	DataDir string
	// KeyFile overrides the key path; default DataDir/KeyFileName.
	KeyFile string
	// Name (≤ 80 chars) and Version (≤ 40 chars) appear in the descriptor.
	Name, Version string
	// Provides lists 1–8 APIs.
	Provides []API
	// Apps lists up to 8 store ids ("<owner>/<app>") that use this server.
	Apps []string
	// Issuer is the Dyner origin whose tokens the server accepts; default
	// DefaultIssuer.
	Issuer string
	// Port and Scheme ("http" default, or "https") are what Announce tells
	// the local agent. Announce needs Port.
	Port   int
	Scheme string
	// AgentURL overrides DefaultAgentURL (tests).
	AgentURL string
	// HTTPClient is used for the JWKS fetch and announcements; default a
	// client with a 10 s timeout.
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Descriptor is the JSON served at DescriptorPath.
type Descriptor struct {
	V            int      `json:"v"`
	InstanceID   string   `json:"instanceId"`
	PublicKeyJWK JWK      `json:"publicKeyJwk"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Provides     []API    `json:"provides"`
	Apps         []string `json:"apps"`
	Issuer       string   `json:"issuer"`
}

// Kit is a configured DynApp-aware server identity. It is safe for
// concurrent use.
type Kit struct {
	cfg        Config
	key        *ecdsa.PrivateKey
	descriptor Descriptor
	body       []byte
	provides   map[string]bool
	client     *http.Client
	now        func() time.Time
	jwks       *jwksCache
}

var apiID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}/[a-z0-9][a-z0-9._-]{0,63}$`)

// New validates cfg, loads or creates the instance key and returns the kit.
func New(cfg Config) (*Kit, error) {
	if cfg.Issuer == "" {
		cfg.Issuer = DefaultIssuer
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.Scheme == "" {
		cfg.Scheme = "http"
	}
	if cfg.AgentURL == "" {
		cfg.AgentURL = DefaultAgentURL
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	path := cfg.KeyFile
	if path == "" {
		path = filepath.Join(cfg.DataDir, KeyFileName)
	}
	key, err := loadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	k := &Kit{cfg: cfg, key: key, provides: map[string]bool{}, client: cfg.HTTPClient, now: cfg.Now}
	if k.client == nil {
		k.client = &http.Client{Timeout: 10 * time.Second}
	}
	if k.now == nil {
		k.now = time.Now
	}
	for _, a := range cfg.Provides {
		k.provides[a.API] = true
	}
	jwk := PublicJWK(&key.PublicKey)
	apps := cfg.Apps
	if apps == nil {
		apps = []string{}
	}
	k.descriptor = Descriptor{V: 1, InstanceID: Thumbprint(jwk), PublicKeyJWK: jwk, Name: cfg.Name,
		Version: cfg.Version, Provides: append([]API(nil), cfg.Provides...), Apps: append([]string{}, apps...), Issuer: cfg.Issuer}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "Form & Fuel" stays readable
	if err = enc.Encode(k.descriptor); err != nil {
		return nil, err
	}
	k.body = bytes.TrimSpace(buf.Bytes())
	k.jwks = &jwksCache{url: cfg.Issuer + "/.well-known/jwks.json", client: k.client, now: k.now}
	return k, nil
}

func validate(cfg Config) error {
	switch {
	case cfg.DataDir == "" && cfg.KeyFile == "":
		return errors.New("serverkit: DataDir is required")
	case strings.TrimSpace(cfg.Name) == "" || len([]rune(cfg.Name)) > 80:
		return errors.New("serverkit: Name must be 1–80 characters")
	case len([]rune(cfg.Version)) > 40:
		return errors.New("serverkit: Version must be at most 40 characters")
	case len(cfg.Provides) < 1 || len(cfg.Provides) > 8:
		return errors.New("serverkit: Provides must list 1–8 APIs")
	case len(cfg.Apps) > 8:
		return errors.New("serverkit: Apps may list at most 8 store ids")
	case cfg.Scheme != "http" && cfg.Scheme != "https":
		return errors.New(`serverkit: Scheme must be "http" or "https"`)
	case cfg.Port < 0 || cfg.Port > 65535:
		return errors.New("serverkit: Port must be 0–65535")
	case !strings.HasPrefix(cfg.Issuer, "https://") && !strings.HasPrefix(cfg.Issuer, "http://"):
		return errors.New("serverkit: Issuer must be an http(s) origin")
	}
	for _, a := range cfg.Provides {
		if !apiID.MatchString(a.API) || a.V < 1 {
			return fmt.Errorf("serverkit: invalid API %q v%d", a.API, a.V)
		}
	}
	for _, app := range cfg.Apps {
		if !apiID.MatchString(app) {
			return fmt.Errorf("serverkit: invalid app store id %q", app)
		}
	}
	return nil
}

// InstanceID is the RFC 7638 thumbprint of the instance public key.
func (k *Kit) InstanceID() string { return k.descriptor.InstanceID }

// Audience is the token audience this server accepts:
// "dynapp:server/<instanceId>".
func (k *Kit) Audience() string { return "dynapp:server/" + k.descriptor.InstanceID }

// Descriptor returns a copy of the served descriptor.
func (k *Kit) Descriptor() Descriptor {
	d := k.descriptor
	d.Provides = append([]API(nil), d.Provides...)
	d.Apps = append([]string{}, d.Apps...)
	return d
}

// Handler serves DescriptorPath and ChallengePath without authentication and
// answers 404 for any other path. Mount it ahead of authenticated routes, for
// example mux.Handle("/.well-known/dynapp-server", h) and
// mux.Handle("/.well-known/dynapp-server/", h).
func (k *Kit) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case DescriptorPath:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(k.body)
			}
		case ChallengePath:
			k.serveChallenge(w, r)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	})
}

// Handles reports whether path is one of the kit's unauthenticated endpoints.
func Handles(path string) bool { return path == DescriptorPath || path == ChallengePath }

type challengeRequest struct {
	Nonce   string `json:"nonce"`
	Address string `json:"address"`
}

// ChallengeResponse is the JSON answer to a challenge.
type ChallengeResponse struct {
	InstanceID string `json:"instanceId"`
	Signature  string `json:"signature"`
}

func (k *Kit) serveChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req challengeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid challenge"})
		return
	}
	sig, err := k.Sign(req.Nonce, req.Address)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ChallengeResponse{InstanceID: k.InstanceID(), Signature: sig})
}

// ChallengeMessage is the exact byte string a challenge signs.
func ChallengeMessage(nonce, address string) []byte {
	return []byte(challengePrefix + nonce + "\n" + address)
}

func checkChallenge(nonce, address string) error {
	raw, err := b64.DecodeString(nonce)
	if err != nil || len(raw) < 16 || len(raw) > 64 {
		return errors.New("nonce must be base64url encoding 16–64 bytes")
	}
	if address == "" || len(address) > 255 || strings.ContainsAny(address, "\r\n\x00 /") {
		return errors.New("address must be the host:port as dialed")
	}
	return nil
}

// Sign answers a challenge: an ES256 signature (P1363 r||s, base64url) over
// ChallengeMessage(nonce, address).
func (k *Kit) Sign(nonce, address string) (string, error) {
	if err := checkChallenge(nonce, address); err != nil {
		return "", err
	}
	digest := sha256.Sum256(ChallengeMessage(nonce, address))
	r, s, err := ecdsa.Sign(rand.Reader, k.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return b64.EncodeToString(sig), nil
}

// VerifyChallenge checks a challenge signature against a descriptor's public
// key, as a verifier (the agent) does. It is exported for clients and tests.
func VerifyChallenge(pub JWK, nonce, address, signature string) bool {
	key, err := jwkToKey(pub.Kty, pub.Crv, pub.X, pub.Y)
	if err != nil {
		return false
	}
	return verifyP1363(key, ChallengeMessage(nonce, address), signature)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
