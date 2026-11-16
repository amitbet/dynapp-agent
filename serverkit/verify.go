package serverkit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity is the verified caller named by a server-scoped token.
type Identity struct {
	Sub         string `json:"sub"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
	App         string `json:"app,omitempty"`
	Role        string `json:"role"` // "owner", "member" or "guest"
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
	API         string `json:"api"`
}

var (
	// ErrNoToken means the request has no Bearer token.
	ErrNoToken = errors.New("serverkit: missing bearer token")
	// ErrInvalidToken means the token is not valid for this server.
	ErrInvalidToken = errors.New("serverkit: invalid identity token")
	// ErrKeysUnavailable means the issuer's JWKS could not be loaded.
	ErrKeysUnavailable = errors.New("serverkit: issuer keys unavailable")
)

const (
	skew           = 30 * time.Second
	refetchEvery   = time.Minute
	keysMaxAge     = 6 * time.Hour
	audiencePrefix = "dynapp:server/"
)

// BearerToken returns the token from "Authorization: Bearer <token>".
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	t := strings.TrimSpace(h[7:])
	return t, t != ""
}

// ServerScoped reports, without verifying anything, whether a token's
// audience is a server audience ("dynapp:server/…"). Servers that also accept
// origin-audience tokens during migration use it to pick a verifier.
func ServerScoped(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := b64.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	var c struct {
		Aud json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return false
	}
	for _, a := range audiences(c.Aud) {
		if strings.HasPrefix(a, audiencePrefix) {
			return true
		}
	}
	return false
}

// Verify checks the request's Bearer token (section 1, "Accepting DynApp
// logins") and returns the identity it names.
func (k *Kit) Verify(r *http.Request) (Identity, error) {
	token, ok := BearerToken(r)
	if !ok {
		return Identity{}, ErrNoToken
	}
	return k.VerifyToken(token)
}

// VerifyToken checks a server-scoped identity token: ES256 signature by a key
// in <issuer>/.well-known/jwks.json, iss, aud "dynapp:server/<instanceId>",
// exp and nbf with 30 s skew, and api among the provided APIs.
func (k *Kit) VerifyToken(token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &header) != nil || header.Alg != "ES256" || header.Kid == "" {
		return Identity{}, ErrInvalidToken
	}
	pub, err := k.jwks.key(header.Kid)
	if err != nil {
		return Identity{}, err
	}
	if !verifyP1363(pub, []byte(parts[0]+"."+parts[1]), parts[2]) {
		return Identity{}, ErrInvalidToken
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return Identity{}, ErrInvalidToken
	}
	var c struct {
		Iss         string          `json:"iss"`
		Sub         string          `json:"sub"`
		Email       string          `json:"email"`
		Name        string          `json:"name"`
		App         string          `json:"app"`
		Aud         json.RawMessage `json:"aud"`
		Exp         *float64        `json:"exp"`
		Nbf         *float64        `json:"nbf"`
		API         string          `json:"api"`
		Role        string          `json:"role"`
		Environment string          `json:"environment"`
		Service     string          `json:"service"`
	}
	if json.Unmarshal(pb, &c) != nil {
		return Identity{}, ErrInvalidToken
	}
	now := float64(k.now().Unix())
	s := skew.Seconds()
	switch {
	case c.Iss != k.cfg.Issuer:
		return Identity{}, fmt.Errorf("%w: issuer", ErrInvalidToken)
	case !contains(audiences(c.Aud), k.Audience()):
		return Identity{}, fmt.Errorf("%w: audience", ErrInvalidToken)
	case c.Exp == nil || *c.Exp+s < now:
		return Identity{}, fmt.Errorf("%w: expired", ErrInvalidToken)
	case c.Nbf != nil && *c.Nbf-s > now:
		return Identity{}, fmt.Errorf("%w: not yet valid", ErrInvalidToken)
	case !k.provides[c.API]:
		return Identity{}, fmt.Errorf("%w: api not provided", ErrInvalidToken)
	case c.Sub == "":
		return Identity{}, fmt.Errorf("%w: subject", ErrInvalidToken)
	case c.Role != "owner" && c.Role != "member" && c.Role != "guest":
		return Identity{}, fmt.Errorf("%w: role", ErrInvalidToken)
	}
	return Identity{Sub: c.Sub, Email: c.Email, Name: c.Name, App: c.App, Role: c.Role,
		Environment: c.Environment, Service: c.Service, API: c.API}, nil
}

type identityKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the identity Middleware stored in ctx.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Middleware verifies every request's token and passes it on with the
// identity in its context (IdentityFrom). It answers 401 for a missing or
// invalid token and 503 when the issuer's keys cannot be loaded. It does not
// exempt the well-known paths; serve Handler outside it.
func (k *Kit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := k.Verify(r)
		if errors.Is(err, ErrKeysUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity keys unavailable"})
			return
		}
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dynapp"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in to DynApp"})
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

func audiences(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func verifyP1363(pub *ecdsa.PublicKey, msg []byte, signature string) bool {
	sig, err := b64.DecodeString(strings.TrimRight(signature, "="))
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256(msg)
	return ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func jwkToKey(kty, crv, xs, ys string) (*ecdsa.PublicKey, error) {
	if kty != "EC" || crv != "P-256" {
		return nil, errors.New("not a P-256 key")
	}
	x, e1 := b64.DecodeString(strings.TrimRight(xs, "="))
	y, e2 := b64.DecodeString(strings.TrimRight(ys, "="))
	if e1 != nil || e2 != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("bad coordinates")
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("point not on curve")
	}
	return pub, nil
}

// jwksCache holds the issuer's signing keys. An unknown kid triggers a
// refetch at most once a minute; the set is also refreshed when older than
// six hours (a failed refresh keeps the old keys).
type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]*ecdsa.PublicKey
	attempted time.Time
	fetched   time.Time
}

func (c *jwksCache) key(kid string) (*ecdsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	k := c.keys[kid]
	stale := c.keys != nil && now.Sub(c.fetched) > keysMaxAge
	if (k == nil || stale) && (c.attempted.IsZero() || now.Sub(c.attempted) >= refetchEvery) {
		c.attempted = now
		keys, err := c.fetch()
		switch {
		case err == nil:
			c.keys, c.fetched = keys, now
			k = keys[kid]
		case k == nil && c.keys == nil:
			return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
		}
	}
	if k == nil && c.keys == nil {
		return nil, ErrKeysUnavailable
	}
	if k == nil {
		return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
	}
	return k, nil
}

func (c *jwksCache) fetch() (map[string]*ecdsa.PublicKey, error) {
	res, err := c.client.Get(c.url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", res.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty, Crv, X, Y, Kid, Alg, Use string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*ecdsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kid == "" || (jk.Alg != "" && jk.Alg != "ES256") || (jk.Use != "" && jk.Use != "sig") {
			continue
		}
		if pub, err := jwkToKey(jk.Kty, jk.Crv, jk.X, jk.Y); err == nil {
			keys[jk.Kid] = pub
		}
	}
	return keys, nil
}
