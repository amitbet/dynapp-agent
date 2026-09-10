package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	webtransport "github.com/quic-go/webtransport-go"
	"nhooyr.io/websocket"
)

func TestLANCertificateIsEndEntity(t *testing.T) {
	certificate, err := (&Server{StateDir: t.TempDir()}).loadOrCreateLANCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if certificate.TLS.Leaf == nil {
		t.Fatal("LAN certificate leaf was not parsed")
	}
	if certificate.TLS.Leaf.IsCA {
		t.Fatal("LAN WebTransport certificate must be an end-entity certificate")
	}
	if certificate.TLS.Leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Fatal("LAN WebTransport certificate must not sign certificates")
	}
}

func TestLANWebTransportProtocolV2AndReliableChannel(t *testing.T) {
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
	identity := BrowserIdentity{KeyID: BrowserKeyID(key), PublicKeyJWK: key, Origin: "https://dyner.example", Capabilities: []string{"fs.home"}}
	config := Config{EnvironmentID: "env_lan", LANEnabled: true, LANAddress: fmt.Sprintf("127.0.0.1:%d", port), BrowserIdentities: []BrowserIdentity{identity}}
	server := &Server{StateDir: t.TempDir(), Config: config}
	closeLAN, err := server.startLAN(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLAN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialer := webtransport.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{"Origin": []string{identity.Origin}}
	_, session, err := dialer.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d%s", port, RemotePath), headers)
	if err != nil {
		t.Fatal(err)
	}
	defer session.CloseWithError(0, "")
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	socket := &webTransportRecordSocket{session: session, stream: stream}
	initWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-init", "version": 1})
	if err := socket.Write(ctx, websocket.MessageText, initWire); err != nil {
		t.Fatal(err)
	}
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
	authWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": identity.KeyID, "origin": identity.Origin, "signature": base64.RawURLEncoding.EncodeToString(signature)})
	if err := socket.Write(ctx, websocket.MessageText, authWire); err != nil {
		t.Fatal(err)
	}
	helloWire, _ := json.Marshal(map[string]any{"type": "hello", "protocol": ProtocolVersion})
	if err := socket.Write(ctx, websocket.MessageText, helloWire); err != nil {
		t.Fatal(err)
	}
	_, response, err := socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	if err := json.Unmarshal(response, &hello); err != nil {
		t.Fatal(err)
	}
	features := hello["features"].(map[string]any)
	if hello["protocol"] != float64(ProtocolVersion) || features["reliableStreams"] != true {
		t.Fatalf("LAN hello = %#v", hello)
	}
	if !reflect.DeepEqual(hello["capabilities"], []any{"fs.home"}) {
		t.Fatalf("LAN capabilities = %#v", hello["capabilities"])
	}
	requestWire, _ := json.Marshal(map[string]any{"type": "exec", "id": "denied", "file": "echo", "args": []any{"unsafe"}})
	if err := socket.Write(ctx, websocket.MessageText, requestWire); err != nil {
		t.Fatal(err)
	}
	_, response, err = socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var denied map[string]any
	if json.Unmarshal(response, &denied) != nil || denied["type"] != "exec-error" {
		t.Fatalf("scoped pairing result = %s", response)
	}
	server.mu.Lock()
	server.Config.BrowserIdentities = nil
	server.mu.Unlock()
	revokedWire, _ := json.Marshal(map[string]any{"type": "fs", "id": "revoked", "method": "home", "args": []any{}})
	if err := socket.Write(ctx, websocket.MessageText, revokedWire); err != nil {
		t.Fatal(err)
	}
	if _, _, err := socket.Read(ctx); err == nil {
		t.Fatal("revoked browser identity kept an active LAN session")
	}
}

func TestLANWebTransportProbeLeavesSessionReusable(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.LocalAddr().(*net.UDPAddr).Port
	_ = listener.Close()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := BrowserJWK{KTY: "EC", CRV: "P-256", X: base64.RawURLEncoding.EncodeToString(private.PublicKey.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(private.PublicKey.Y.FillBytes(make([]byte, 32)))}
	identity := BrowserIdentity{KeyID: BrowserKeyID(key), PublicKeyJWK: key, Origin: "https://dyner.example", Capabilities: []string{"fs.home"}}
	config := Config{EnvironmentID: "env_lan", LANEnabled: true, LANAddress: fmt.Sprintf("127.0.0.1:%d", port), BrowserIdentities: []BrowserIdentity{identity}}
	server := &Server{StateDir: t.TempDir(), Config: config}
	closeLAN, err := server.startLAN(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLAN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialer := webtransport.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{"Origin": []string{identity.Origin}}
	endpoint := fmt.Sprintf("https://127.0.0.1:%d%s", port, RemotePath)
	_, probeSession, err := dialer.Dial(ctx, endpoint, headers)
	if err != nil {
		t.Fatal(err)
	}
	probeStream, err := probeSession.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	probeSocket := &webTransportRecordSocket{session: probeSession, stream: probeStream}
	probeWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-probe", "version": 1})
	if err := probeSocket.Write(ctx, websocket.MessageText, probeWire); err != nil {
		t.Fatal(err)
	}
	_, challengeWire, err := probeSocket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var challenge struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(challengeWire, &challenge) != nil || challenge.Type != "dynapp-direct-local-challenge" {
		t.Fatalf("probe challenge = %s", challengeWire)
	}
	_ = probeSession.CloseWithError(0, "")

	_, session, err := dialer.Dial(ctx, endpoint, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer session.CloseWithError(0, "")
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	socket := &webTransportRecordSocket{session: session, stream: stream}
	initWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-init", "version": 1})
	if err := socket.Write(ctx, websocket.MessageText, initWire); err != nil {
		t.Fatal(err)
	}
	_, challengeWire, err = socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var authChallenge struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(challengeWire, &authChallenge); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(directLocalPayload(authChallenge.Nonce, identity.Origin, identity.KeyID))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	authWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": identity.KeyID, "origin": identity.Origin, "signature": base64.RawURLEncoding.EncodeToString(signature)})
	if err := socket.Write(ctx, websocket.MessageText, authWire); err != nil {
		t.Fatal(err)
	}
	helloWire, _ := json.Marshal(map[string]any{"type": "hello", "protocol": ProtocolVersion})
	if err := socket.Write(ctx, websocket.MessageText, helloWire); err != nil {
		t.Fatal(err)
	}
	_, response, err := socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	if err := json.Unmarshal(response, &hello); err != nil || hello["type"] != "hello" || hello["ok"] != true {
		t.Fatalf("hello after probe = %s", response)
	}
}

func TestLANWebTransportTestModePairsUnknownBrowserWithoutPrompt(t *testing.T) {
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
	origin := "http://localhost:5179"
	keyID := BrowserKeyID(key)
	config := Config{EnvironmentID: "env_local", LANEnabled: true, LANAddress: fmt.Sprintf("127.0.0.1:%d", port)}
	server := &Server{StateDir: t.TempDir(), Config: config, autoApprovePairings: true}
	closeLAN, err := server.startLAN(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLAN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialer := webtransport.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := http.Header{"Origin": []string{origin}}
	_, session, err := dialer.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d%s", port, RemotePath), headers)
	if err != nil {
		t.Fatal(err)
	}
	defer session.CloseWithError(0, "")
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	socket := &webTransportRecordSocket{session: session, stream: stream}
	initWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-init", "version": 2, "publicKeyJwk": key, "app": map[string]any{"storeId": "owner/app", "declaredPermissions": []string{"fs.home", "fs.exec"}}})
	if err := socket.Write(ctx, websocket.MessageText, initWire); err != nil {
		t.Fatal(err)
	}
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
	digest := sha256.Sum256(directLocalPayload(challenge.Nonce, origin, keyID))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	authWire, _ := json.Marshal(map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": keyID, "origin": origin, "signature": base64.RawURLEncoding.EncodeToString(signature)})
	if err := socket.Write(ctx, websocket.MessageText, authWire); err != nil {
		t.Fatal(err)
	}
	helloWire, _ := json.Marshal(map[string]any{"type": "hello", "protocol": ProtocolVersion})
	if err := socket.Write(ctx, websocket.MessageText, helloWire); err != nil {
		t.Fatal(err)
	}
	_, response, err := socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	if err := json.Unmarshal(response, &hello); err != nil || hello["ok"] != true {
		t.Fatalf("hello after approval = %s", response)
	}
	if !reflect.DeepEqual(hello["capabilities"], []any{"fs.exec", "fs.home"}) {
		t.Fatalf("capabilities = %#v", hello["capabilities"])
	}
	server.mu.Lock()
	identities := append([]BrowserIdentity(nil), server.Config.BrowserIdentities...)
	server.mu.Unlock()
	if len(identities) != 1 || identities[0].StoreID != "owner/app" || identities[0].ApprovedAt == 0 || !reflect.DeepEqual(identities[0].Capabilities, []string{"fs.exec", "fs.home"}) {
		t.Fatalf("stored identity = %#v", identities)
	}
}

func TestClassifyOriginAllowlist(t *testing.T) {
	config := Config{DynerBaseURL: "https://dynapp.io"}
	identities := []BrowserIdentity{{Origin: "https://dyner.example"}}
	cases := map[string]originKind{
		"https://dynapp.io":                   originHosted,
		"https://amitbet-commander.dynapp.io": originHosted,
		"http://localhost:5179":               originDevelopment,
		"http://127.0.0.1:5200":               originDevelopment,
		"https://dyner.example":               originIdentity,
		"https://evil.example":                originRejected,
		"http://dyner.example":                originRejected,
		"https://dynapp.io.evil.example":      originRejected,
		"https://dynapp.io/path":              originRejected,
		"":                                    originRejected,
	}
	for origin, want := range cases {
		if got := classifyOrigin(config, identities, origin); got != want {
			t.Fatalf("classifyOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
	lanIdentities := []BrowserIdentity{{Origin: "https://192.168.1.22:9010"}}
	lanConfig := Config{DynerBaseURL: "https://192.168.1.22:9010"}
	if classifyOrigin(lanConfig, lanIdentities, "http://192.168.1.22:9010") != originIdentity {
		t.Fatal("private HTTP Dyner origin should match the HTTPS identity")
	}
	if classifyOrigin(lanConfig, nil, "https://192.168.1.22:9010") != originHosted {
		t.Fatal("the Dyner base origin should count as hosted")
	}
	if classifyOrigin(lanConfig, nil, "https://anything.dynapp.io") != originRejected {
		t.Fatal("an IP-literal Dyner host has no wildcard app domain")
	}
}

func TestSortLanHostsPrefersDynerLANOverTailscale(t *testing.T) {
	hosts := sortLanHosts([]string{"100.67.83.79", "192.168.1.22"}, "192.168.1.22")
	if len(hosts) != 2 || hosts[0] != "192.168.1.22" {
		t.Fatalf("preferred dyner LAN = %#v", hosts)
	}
	hosts = sortLanHosts([]string{"100.67.83.79", "192.168.1.22"}, "")
	if hosts[0] != "192.168.1.22" {
		t.Fatalf("private LAN should beat Tailscale CGNAT = %#v", hosts)
	}
}
