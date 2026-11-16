# serverkit

`github.com/amitbet/dynapp-agent/serverkit` makes a Go HTTP server
DynApp-aware (mesh network Phase 3a, server protocol section 1). It is its own
Go module, uses only the standard library and has no agent dependencies.

It provides:

- the instance key: an ES256 (P-256) key created on first run as a 0600 PEM
  file (`dynapp-server-key.pem`) in your data directory. The instance id is
  its RFC 7638 thumbprint.
- `GET /.well-known/dynapp-server` (descriptor) and
  `POST /.well-known/dynapp-server/challenge` (proof of key, bound to the
  dialed address), both unauthenticated.
- `Announce`: posts `{"port","scheme"}` to the local agent at
  `http://127.0.0.1:9011/v1/servers/announce` on start and every 60 s. A
  missing agent is not an error.
- `Verify` / `Middleware`: checks a server-scoped DynApp identity token
  (ES256 via `<issuer>/.well-known/jwks.json`, `iss`, `aud` =
  `dynapp:server/<instanceId>`, `exp`/`nbf` with 30 s skew, `api` provided)
  and puts the `Identity` in the request context.

mDNS (`_dynapp._tcp`) is not implemented yet. Loopback announcement and the
agent's port probe still find the server.

## Usage

```go
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/amitbet/dynapp-agent/serverkit"
)

func main() {
	kit, err := serverkit.New(serverkit.Config{
		DataDir:  "/var/lib/myserver",
		Name:     "My journal",
		Version:  "1.0.0",
		Provides: []serverkit.API{{API: "me/journal", V: 1}},
		Apps:     []string{"me/journal-app"},
		Port:     9018, // what Announce tells the agent
	})
	if err != nil {
		log.Fatal(err)
	}
	go kit.Announce(context.Background())

	api := http.NewServeMux()
	api.HandleFunc("/v1/notes", func(w http.ResponseWriter, r *http.Request) {
		id, _ := serverkit.IdentityFrom(r.Context())
		// Keep data per id.Sub. id.Role is "owner", "member" or "guest";
		// a guest gets their own data, never the owner's.
		w.Write([]byte("hello " + id.Sub))
	})

	mux := http.NewServeMux()
	mux.Handle(serverkit.DescriptorPath, kit.Handler()) // no auth
	mux.Handle(serverkit.ChallengePath, kit.Handler())  // no auth
	mux.Handle("/", kit.Middleware(api))
	log.Fatal(http.ListenAndServe(":9018", mux))
}
```

The agent fetches the descriptor from `127.0.0.1:<port>`, so listen on
loopback or on all interfaces.

### Migrating from origin-audience tokens

A server that still accepts the older `auth.identity` tokens (audience = the
server's origin) can pick a verifier per request:

```go
token, _ := serverkit.BearerToken(r)
if serverkit.ServerScoped(token) { // unverified peek at aud
	id, err := kit.VerifyToken(token)
	// ...
} else {
	// existing origin-audience verification
}
```

## API

| Symbol | Purpose |
| --- | --- |
| `New(Config) (*Kit, error)` | Validate config, load or create the key |
| `Kit.Handler() http.Handler` | Descriptor and challenge endpoints |
| `Kit.Announce(ctx) error` / `AnnounceOnce(ctx) error` | Loopback announcement loop / single attempt |
| `Kit.Verify(r) (Identity, error)` / `VerifyToken(string)` | Token verification |
| `Kit.Middleware(next) http.Handler` | 401 on a bad token, 503 when JWKS is unreachable |
| `IdentityFrom(ctx)` / `WithIdentity(ctx, id)` | Identity in the request context |
| `Kit.InstanceID()`, `Audience()`, `Descriptor()`, `Sign(nonce, address)` | Instance facts and raw challenge signing |
| `Thumbprint(JWK)`, `PublicJWK(*ecdsa.PublicKey)`, `VerifyChallenge(...)`, `ChallengeMessage(...)` | Helpers for verifiers and tests |
| `BearerToken(r)`, `ServerScoped(token)`, `Handles(path)` | Routing helpers |

Errors: `ErrNoToken`, `ErrInvalidToken` (wrapped with the failing check),
`ErrKeysUnavailable`.

```sh
cd serverkit && go vet ./... && go test ./...
```
