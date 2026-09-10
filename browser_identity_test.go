package shellagent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestBrowserProofBindsNonceOriginAndKey(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := BrowserJWK{
		KTY: "EC", CRV: "P-256",
		X: base64.RawURLEncoding.EncodeToString(private.PublicKey.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(private.PublicKey.Y.FillBytes(make([]byte, 32))),
	}
	identity := BrowserIdentity{KeyID: BrowserKeyID(key), PublicKeyJWK: key, Origin: "https://dyner.example", Capabilities: []string{"fs.home"}}
	nonce := "nonce-a"
	digest := sha256.Sum256(directLocalPayload(nonce, identity.Origin, identity.KeyID))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	if !verifyBrowserProof(identity, nonce, identity.Origin, signature) {
		t.Fatal("valid browser proof was rejected")
	}
	if verifyBrowserProof(identity, "nonce-b", identity.Origin, signature) {
		t.Fatal("browser proof replayed against a different nonce")
	}
	changedOrigin := identity
	changedOrigin.Origin = "https://evil.example"
	if verifyBrowserProof(changedOrigin, nonce, identity.Origin, signature) {
		t.Fatal("browser proof accepted for a different origin")
	}
	changedKeyID := identity
	changedKeyID.KeyID = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if verifyBrowserProof(changedKeyID, nonce, identity.Origin, signature) {
		t.Fatal("browser proof accepted for a different key id")
	}
}

func TestApprovedAppGrantMigratesLegacyOriginGrant(t *testing.T) {
	legacy := BrowserIdentity{
		KeyID:        "legacy",
		Origin:       "https://owner-tool.dynapp.io",
		Capabilities: []string{"net.protocol.https"},
	}
	grant, ok := findApprovedAppGrant([]BrowserIdentity{legacy}, legacy.Origin, "owner/tool")
	if !ok || grant.KeyID != legacy.KeyID {
		t.Fatalf("legacy grant was not reused: %#v, %v", grant, ok)
	}
	if _, ok := findApprovedAppGrant([]BrowserIdentity{legacy}, "https://other.dynapp.io", "owner/tool"); ok {
		t.Fatal("legacy grant crossed origins")
	}
}

func TestApprovedAppGrantPrefersCurrentAppScope(t *testing.T) {
	origin := "https://owner-tool.dynapp.io"
	legacy := BrowserIdentity{KeyID: "legacy", Origin: origin, Capabilities: []string{"net.protocol.https"}}
	current := BrowserIdentity{KeyID: "current", Origin: origin, StoreID: "owner/tool", ApprovedAt: 10, Capabilities: []string{"net.protocol.https"}}
	grant, ok := findApprovedAppGrant([]BrowserIdentity{legacy, current}, origin, "owner/tool")
	if !ok || grant.KeyID != current.KeyID {
		t.Fatalf("current app grant was not preferred: %#v, %v", grant, ok)
	}
}
