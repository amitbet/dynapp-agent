package serverkit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type issuer struct {
	key     *ecdsa.PrivateKey
	kid     string
	srv     *httptest.Server
	fetches atomic.Int32
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	is := &issuer{key: key, kid: "k1"}
	jwk := PublicJWK(&key.PublicKey)
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		is.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": jwk.Kty, "crv": jwk.Crv, "x": jwk.X, "y": jwk.Y, "kid": is.kid, "alg": "ES256", "use": "sig"}}})
	}))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) sign(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "JWT", "kid": kid})
	p, _ := json.Marshal(claims)
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	d := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig)
}

func testKit(t *testing.T, is *issuer, dir string) *Kit {
	t.Helper()
	k, err := New(Config{DataDir: dir, Name: "Form & Fuel journal", Version: "0.11.0",
		Provides: []API{{API: "amit-bet/formfuel-journal", V: 1}}, Apps: []string{"amit-bet/formfuel"},
		Issuer: is.srv.URL, Port: 9018})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func claims(is *issuer, k *Kit) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": is.srv.URL, "sub": "amit-bet", "email": "a@example.com", "name": "Amit",
		"app": "amit-bet/formfuel", "aud": k.Audience(), "api": "amit-bet/formfuel-journal", "role": "owner",
		"environment": "env_1", "service": "svc_1", "iat": now, "nbf": now, "exp": now + 300, "jti": "j1"}
}

func TestDescriptorShapeAndThumbprint(t *testing.T) {
	is := newIssuer(t)
	k := testKit(t, is, t.TempDir())
	rec := httptest.NewRecorder()
	k.Handler().ServeHTTP(rec, httptest.NewRequest("GET", DescriptorPath, nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatal(rec.Code, rec.Header())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"v", "instanceId", "publicKeyJwk", "name", "version", "provides", "apps", "issuer"} {
		if _, ok := raw[f]; !ok {
			t.Fatal("missing field", f)
		}
	}
	var d Descriptor
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.V != 1 || d.Name != "Form & Fuel journal" || d.Version != "0.11.0" || d.Issuer != is.srv.URL ||
		len(d.Provides) != 1 || d.Provides[0] != (API{"amit-bet/formfuel-journal", 1}) || len(d.Apps) != 1 || d.Apps[0] != "amit-bet/formfuel" {
		t.Fatalf("%+v", d)
	}
	if d.PublicKeyJWK.Kty != "EC" || d.PublicKeyJWK.Crv != "P-256" || d.InstanceID != Thumbprint(d.PublicKeyJWK) || d.InstanceID != k.InstanceID() {
		t.Fatalf("%+v", d)
	}
	// Recompute the thumbprint independently with encoding/json (member order is
	// lexicographic for a map) to pin the RFC 7638 canonical form.
	canon, _ := json.Marshal(map[string]string{"kty": "EC", "crv": "P-256", "x": d.PublicKeyJWK.X, "y": d.PublicKeyJWK.Y})
	sum := sha256.Sum256(canon)
	if b64.EncodeToString(sum[:]) != d.InstanceID || len(d.InstanceID) != 43 {
		t.Fatal("thumbprint mismatch", string(canon))
	}
	if k.Audience() != "dynapp:server/"+d.InstanceID {
		t.Fatal(k.Audience())
	}
	rec = httptest.NewRecorder()
	k.Handler().ServeHTTP(rec, httptest.NewRequest("POST", DescriptorPath, nil))
	if rec.Code != 405 {
		t.Fatal(rec.Code)
	}
}

// RFC 7638 section 3.1 example uses an RSA key; this checks the canonical
// string building against a fixed EC JWK so a regression changes the value.
func TestThumbprintCanonicalForm(t *testing.T) {
	j := JWK{Kty: "EC", Crv: "P-256", X: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", Y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU","y":"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}`))
	if Thumbprint(j) != b64.EncodeToString(sum[:]) {
		t.Fatal(Thumbprint(j))
	}
}

func challenge(t *testing.T, k *Kit, body string) (*httptest.ResponseRecorder, ChallengeResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	k.Handler().ServeHTTP(rec, httptest.NewRequest("POST", ChallengePath, strings.NewReader(body)))
	var res ChallengeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func TestChallengeSignatureVerifies(t *testing.T) {
	is := newIssuer(t)
	k := testKit(t, is, t.TempDir())
	nonce := b64.EncodeToString([]byte("0123456789abcdef0123"))
	body, _ := json.Marshal(map[string]string{"nonce": nonce, "address": "127.0.0.1:9018"})
	rec, res := challenge(t, k, string(body))
	if rec.Code != 200 || res.InstanceID != k.InstanceID() || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Code, rec.Body)
	}
	jwk := k.Descriptor().PublicKeyJWK
	if !VerifyChallenge(jwk, nonce, "127.0.0.1:9018", res.Signature) {
		t.Fatal("signature does not verify")
	}
	if VerifyChallenge(jwk, nonce, "192.168.1.81:9018", res.Signature) {
		t.Fatal("signature verifies for another address")
	}
	if VerifyChallenge(jwk, b64.EncodeToString([]byte("another nonce 16b")), "127.0.0.1:9018", res.Signature) {
		t.Fatal("signature verifies for another nonce")
	}
	// The message format is fixed by the protocol.
	if string(ChallengeMessage("n", "a:1")) != "dynapp-server-challenge/v1\nn\na:1" {
		t.Fatal(string(ChallengeMessage("n", "a:1")))
	}
	if sig, _ := b64.DecodeString(res.Signature); len(sig) != 64 {
		t.Fatal("not P1363", len(sig))
	}
}

func TestChallengeRejectsBadInput(t *testing.T) {
	is := newIssuer(t)
	k := testKit(t, is, t.TempDir())
	for name, body := range map[string]string{
		"short nonce":  `{"nonce":"` + b64.EncodeToString(make([]byte, 15)) + `","address":"127.0.0.1:9018"}`,
		"long nonce":   `{"nonce":"` + b64.EncodeToString(make([]byte, 65)) + `","address":"127.0.0.1:9018"}`,
		"padded nonce": `{"nonce":"AAAAAAAAAAAAAAAAAAAAAA==","address":"127.0.0.1:9018"}`,
		"no address":   `{"nonce":"` + b64.EncodeToString(make([]byte, 16)) + `"}`,
		"newline addr": `{"nonce":"` + b64.EncodeToString(make([]byte, 16)) + `","address":"a:1\nb"}`,
		"not json":     `nope`,
	} {
		if rec, _ := challenge(t, k, body); rec.Code != 400 {
			t.Fatal(name, rec.Code)
		}
	}
	for _, n := range []int{16, 64} {
		if rec, _ := challenge(t, k, `{"nonce":"`+b64.EncodeToString(make([]byte, n))+`","address":"[::1]:9018"}`); rec.Code != 200 {
			t.Fatal(n, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	k.Handler().ServeHTTP(rec, httptest.NewRequest("GET", ChallengePath, nil))
	if rec.Code != 405 {
		t.Fatal(rec.Code)
	}
}

func TestKeyPersistsAcrossNew(t *testing.T) {
	is := newIssuer(t)
	dir := filepath.Join(t.TempDir(), "data")
	a := testKit(t, is, dir)
	b := testKit(t, is, dir)
	if a.InstanceID() != b.InstanceID() {
		t.Fatal("instance id changed across New")
	}
	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if c := testKit(t, is, t.TempDir()); c.InstanceID() == a.InstanceID() {
		t.Fatal("a new data directory should get a new key")
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{DataDir: dir, Name: "x", Provides: []API{{"a/b", 1}}}); err == nil {
		t.Fatal("a corrupt key must not be replaced silently")
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	ok := Config{DataDir: dir, Name: "n", Provides: []API{{"a/b", 1}}}
	for name, mutate := range map[string]func(*Config){
		"no name":      func(c *Config) { c.Name = "" },
		"long name":    func(c *Config) { c.Name = strings.Repeat("x", 81) },
		"long version": func(c *Config) { c.Version = strings.Repeat("1", 41) },
		"no provides":  func(c *Config) { c.Provides = nil },
		"9 provides":   func(c *Config) { c.Provides = make([]API, 9) },
		"bad api":      func(c *Config) { c.Provides = []API{{"nope", 1}} },
		"api v0":       func(c *Config) { c.Provides = []API{{"a/b", 0}} },
		"9 apps":       func(c *Config) { c.Apps = []string{"a/a", "a/b", "a/c", "a/d", "a/e", "a/f", "a/g", "a/h", "a/i"} },
		"bad scheme":   func(c *Config) { c.Scheme = "ftp" },
		"no data dir":  func(c *Config) { c.DataDir = "" },
	} {
		c := ok
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	k, err := New(ok)
	if err != nil {
		t.Fatal(err)
	}
	if d := k.Descriptor(); d.Issuer != DefaultIssuer || d.Apps == nil {
		t.Fatalf("%+v", d)
	}
}

func TestVerifyHappyPathAndMiddleware(t *testing.T) {
	is := newIssuer(t)
	k := testKit(t, is, t.TempDir())
	token := is.sign(t, is.key, is.kid, claims(is, k))
	h := k.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok {
			t.Fatal("no identity in context")
		}
		_ = json.NewEncoder(w).Encode(id)
	}))
	req := httptest.NewRequest("GET", "/v1/records", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var id Identity
	_ = json.Unmarshal(rec.Body.Bytes(), &id)
	want := Identity{Sub: "amit-bet", Email: "a@example.com", Name: "Amit", App: "amit-bet/formfuel", Role: "owner",
		Environment: "env_1", Service: "svc_1", API: "amit-bet/formfuel-journal"}
	if rec.Code != 200 || id != want {
		t.Fatal(rec.Code, rec.Body)
	}
	if !ServerScoped(token) {
		t.Fatal("ServerScoped")
	}
	// Audience as an array also matches.
	c := claims(is, k)
	c["aud"] = []string{"other", k.Audience()}
	if _, err := k.VerifyToken(is.sign(t, is.key, is.kid, c)); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/records", nil))
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("no token", rec.Code)
	}
	if is.fetches.Load() != 1 {
		t.Fatal("keys should be cached, fetches:", is.fetches.Load())
	}
}

func TestVerifyRejections(t *testing.T) {
	is := newIssuer(t)
	k := testKit(t, is, t.TempDir())
	now := time.Now().Unix()
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cases := map[string]string{}
	for name, change := range map[string][2]any{
		"wrong aud":     {"aud", "dynapp:server/someone-else"},
		"origin aud":    {"aud", "http://192.168.1.81:9018"},
		"wrong iss":     {"iss", "https://evil.example"},
		"expired":       {"exp", now - 31},
		"no exp":        {"exp", nil},
		"not yet valid": {"nbf", now + 31},
		"unknown api":   {"api", "amit-bet/other-api"},
		"no api":        {"api", ""},
		"no sub":        {"sub", ""},
		"unknown role":  {"role", "admin"},
	} {
		c := claims(is, k)
		if change[1] == nil {
			delete(c, change[0].(string))
		} else {
			c[change[0].(string)] = change[1]
		}
		cases[name] = is.sign(t, is.key, is.kid, c)
	}
	good := is.sign(t, is.key, is.kid, claims(is, k))
	parts := strings.Split(good, ".")
	tampered, _ := json.Marshal(map[string]any{"iss": is.srv.URL, "sub": "mallory", "aud": k.Audience(), "api": "amit-bet/formfuel-journal", "role": "owner", "exp": now + 300})
	cases["bad sig (other key)"] = is.sign(t, other, is.kid, claims(is, k))
	cases["bad sig (tampered payload)"] = parts[0] + "." + b64.EncodeToString(tampered) + "." + parts[2]
	cases["unknown kid"] = is.sign(t, is.key, "k2", claims(is, k))
	cases["not a jwt"] = "abc"
	for name, token := range cases {
		if _, err := k.VerifyToken(token); !errors.Is(err, ErrInvalidToken) {
			t.Fatal(name, "accepted or wrong error:", err)
		}
	}
	// Within skew: expired 20 s ago and nbf 20 s ahead are still fine.
	c := claims(is, k)
	c["exp"], c["nbf"] = now-20, now+20
	if _, err := k.VerifyToken(is.sign(t, is.key, is.kid, c)); err != nil {
		t.Fatal("skew", err)
	}
}

func TestUnknownKidRefetchedAtMostOncePerMinute(t *testing.T) {
	is := newIssuer(t)
	clock := time.Now()
	k, err := New(Config{DataDir: t.TempDir(), Name: "n", Provides: []API{{"amit-bet/formfuel-journal", 1}},
		Issuer: is.srv.URL, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	c := claims(is, k)
	c["exp"] = clock.Unix() + 3600
	c["nbf"] = clock.Unix()
	unknown := is.sign(t, is.key, "rotated", c)
	for i := 0; i < 5; i++ {
		_, _ = k.VerifyToken(unknown)
	}
	if n := is.fetches.Load(); n != 1 {
		t.Fatal("fetches", n)
	}
	clock = clock.Add(61 * time.Second)
	is.kid = "rotated" // the issuer rotates; the next allowed refetch finds it
	if _, err := k.VerifyToken(unknown); err != nil || is.fetches.Load() != 2 {
		t.Fatal(err, is.fetches.Load())
	}
}

func TestKeysUnavailable(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer down.Close()
	k, err := New(Config{DataDir: t.TempDir(), Name: "n", Provides: []API{{"a/b", 1}}, Issuer: down.URL})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer a.b.c")
		k.Middleware(http.NotFoundHandler()).ServeHTTP(rec, req)
		hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "x"})
		req.Header.Set("Authorization", "Bearer "+b64.EncodeToString(hdr)+".e30.AAAA")
		rec = httptest.NewRecorder()
		k.Middleware(http.NotFoundHandler()).ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatal(i, rec.Code)
		}
	}
}

func TestAnnounce(t *testing.T) {
	is := newIssuer(t)
	got := make(chan map[string]any, 4)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/servers/announce" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
		w.WriteHeader(202)
	}))
	defer agent.Close()
	k, err := New(Config{DataDir: t.TempDir(), Name: "n", Provides: []API{{"a/b", 1}}, Issuer: is.srv.URL, Port: 9018, AgentURL: agent.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Announce(ctx) }()
	select {
	case body := <-got:
		if len(body) != 2 || body["port"] != float64(9018) || body["scheme"] != "http" {
			t.Fatal(body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no announcement")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// No agent listening: AnnounceOnce reports it, Announce keeps going quietly.
	agent.Close()
	if err := k.AnnounceOnce(context.Background()); err == nil {
		t.Fatal("expected an error with no agent")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := k.Announce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	noPort, _ := New(Config{DataDir: t.TempDir(), Name: "n", Provides: []API{{"a/b", 1}}})
	if err := noPort.Announce(context.Background()); err == nil {
		t.Fatal("announce without a port")
	}
}
