package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	webtransport "github.com/quic-go/webtransport-go"
	"nhooyr.io/websocket"
)

// dialAuthenticatedLAN starts a LAN server and returns a session whose main
// stream has completed the direct-local challenge and the protocol hello.
func dialAuthenticatedLAN(t *testing.T, capabilities []string) (*Server, *webtransport.Session, context.Context) {
	t.Helper()
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := BrowserJWK{KTY: "EC", CRV: "P-256", X: base64.RawURLEncoding.EncodeToString(private.PublicKey.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(private.PublicKey.Y.FillBytes(make([]byte, 32)))}
	identity := BrowserIdentity{KeyID: BrowserKeyID(key), PublicKeyJWK: key, Origin: "https://dyner.example", Capabilities: capabilities}
	config := Config{EnvironmentID: "env_lan", LANEnabled: true, LANAddress: fmt.Sprintf("127.0.0.1:%d", port), BrowserIdentities: []BrowserIdentity{identity}}
	server := &Server{StateDir: t.TempDir(), Config: config}
	closeLAN, err := server.startLAN(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeLAN() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	dialer := webtransport.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	_, session, err := dialer.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d%s", port, RemotePath), http.Header{"Origin": []string{identity.Origin}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.CloseWithError(0, "") })
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	socket := &webTransportRecordSocket{session: session, stream: stream}
	write := func(value map[string]any) {
		wire, _ := json.Marshal(value)
		if err := socket.Write(ctx, websocket.MessageText, wire); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"type": "dynapp-direct-local-init", "version": 1})
	_, challengeWire, err := socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var challenge struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(challengeWire, &challenge); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(directLocalPayload(challenge.Nonce, identity.Origin, identity.KeyID))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	write(map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": identity.KeyID, "origin": identity.Origin, "signature": base64.RawURLEncoding.EncodeToString(signature)})
	write(map[string]any{"type": "hello", "protocol": ProtocolVersion})
	if _, _, err := socket.Read(ctx); err != nil {
		t.Fatal(err)
	}
	return server, session, ctx
}

// A browser copies a large file as hundreds of chunk requests, each on a
// stream of its own that it closes once the reply is in. The agent must
// finish its side of every stream too, or the peer's stream credit is never
// returned and the browser fails to open a stream partway through the copy.
func TestLANFilesystemStreamsAreReleasedAfterEachChunk(t *testing.T) {
	_, session, ctx := dialAuthenticatedLAN(t, []string{"fs.readChunk"})
	chunkPath := filepath.Join(t.TempDir(), "chunk.bin")
	if err := os.WriteFile(chunkPath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// quic-go admits 100 incoming streams at a time by default.
	for index := 0; index < 150; index++ {
		openCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		stream, err := session.OpenStreamSync(openCtx)
		cancel()
		if err != nil {
			t.Fatalf("stream %d: open failed: %v", index, err)
		}
		socket := &webTransportRecordSocket{session: session, stream: stream}
		id := fmt.Sprintf("fs-%d", index)
		bind, _ := json.Marshal(map[string]any{"type": "channel", "action": "bind", "kind": "fs", "protocol": ProtocolVersion, "id": id})
		if err := socket.Write(ctx, websocket.MessageText, bind); err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(map[string]any{"type": "fs", "id": id, "method": "readChunkBinary", "args": []any{chunkPath, 0, 4096}})
		if err := socket.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		kind, response, err := socket.Read(ctx)
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("stream %d: response kind = %v, err = %v", index, kind, err)
		}
		if header, _, err := decodeBinaryFrame(response); err != nil || header.Type != "fs-result" {
			t.Fatalf("stream %d: response = %#v, %v", index, header, err)
		}
		// The browser closes its writer after the reply and drains the reader.
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, stream)
	}
}
