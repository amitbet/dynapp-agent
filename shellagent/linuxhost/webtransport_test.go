package linuxhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

type wtTestServer struct {
	url     string
	hash    string
	origins chan string
}

// startEchoServer serves WebTransport on loopback: each stream echoes what it
// reads, and a session whose first stream sends "close" is closed with code 7.
func startEchoServer(t *testing.T) *wtTestServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lan"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	origins := make(chan string, 4)
	mux := http.NewServeMux()
	server := &webtransport.Server{
		H3:          &http3.Server{TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}), Handler: mux},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(server.H3)
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		origins <- r.Header.Get("Origin")
		session, err := server.Upgrade(w, r)
		if err != nil {
			return
		}
		go func() {
			for {
				stream, err := session.AcceptStream(context.Background())
				if err != nil {
					return
				}
				go func() {
					data, _ := io.ReadAll(stream)
					if string(data) == "close" {
						_ = session.CloseWithError(7, "bye")
						return
					}
					_, _ = stream.Write(data)
					_ = stream.Close()
				}()
			}
		}()
	})
	go func() { _ = server.Serve(packet) }()
	t.Cleanup(func() { _ = server.Close() })
	return &wtTestServer{
		url:     fmt.Sprintf("https://127.0.0.1:%d/wt", packet.LocalAddr().(*net.UDPAddr).Port),
		hash:    base64.StdEncoding.EncodeToString(sum[:]),
		origins: origins,
	}
}

type eventLog chan map[string]any

func newBridge(t *testing.T) (*WebTransportBridge, eventLog) {
	t.Helper()
	events := make(eventLog, 256)
	bridge := &WebTransportBridge{
		Origin:       "https://amitbet-formfuel.dynapp.io",
		Deliver:      func(event map[string]any) { events <- event },
		AllowAddress: func(ip net.IP) bool { return ip.IsLoopback() },
	}
	t.Cleanup(bridge.Shutdown)
	return bridge, events
}

func (events eventLog) next(t *testing.T, want string) map[string]any {
	t.Helper()
	select {
	case event := <-events:
		if event["event"] != want {
			t.Fatalf("event = %v, want %q", event, want)
		}
		return event
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q event", want)
		return nil
	}
}

func write(data string) string { return base64.StdEncoding.EncodeToString([]byte(data)) }

func TestWebTransportBridgeEchoesAStream(t *testing.T) {
	server := startEchoServer(t)
	bridge, events := newBridge(t)
	bridge.Handle(BridgeMessage{Op: "wt-open", WID: "w1", URL: server.url, Hashes: []string{server.hash}})
	// A stream requested before the session is ready waits for it.
	bridge.Handle(BridgeMessage{Op: "wt-stream", WID: "w1", Stream: "s1"})
	events.next(t, "ready")
	if origin := <-server.origins; origin != "https://amitbet-formfuel.dynapp.io" {
		t.Fatalf("Origin = %q", origin)
	}
	events.next(t, "stream-ready")
	bridge.Handle(BridgeMessage{Op: "wt-write", WID: "w1", Stream: "s1", Binary: write("hello ")})
	events.next(t, "ack")
	bridge.Handle(BridgeMessage{Op: "wt-write", WID: "w1", Stream: "s1", Binary: write("LAN")})
	events.next(t, "ack")
	bridge.Handle(BridgeMessage{Op: "wt-fin", WID: "w1", Stream: "s1"})
	events.next(t, "fin")
	var received strings.Builder
	for {
		bridge.Handle(BridgeMessage{Op: "wt-read", WID: "w1", Stream: "s1"})
		event := <-events
		if event["event"] == "end" {
			break
		}
		if event["event"] != "data" {
			t.Fatalf("read event = %v", event)
		}
		data, _ := base64.StdEncoding.DecodeString(event["binary"].(string))
		received.Write(data)
	}
	if received.String() != "hello LAN" {
		t.Fatalf("echo = %q", received.String())
	}
	bridge.Handle(BridgeMessage{Op: "wt-close", WID: "w1", Code: 0})
	events.next(t, "closed")
}

func TestWebTransportBridgeReportsTheServersCloseCode(t *testing.T) {
	server := startEchoServer(t)
	bridge, events := newBridge(t)
	bridge.Handle(BridgeMessage{Op: "wt-open", WID: "w1", URL: server.url, Hashes: []string{server.hash}})
	events.next(t, "ready")
	bridge.Handle(BridgeMessage{Op: "wt-stream", WID: "w1", Stream: "s1"})
	events.next(t, "stream-ready")
	bridge.Handle(BridgeMessage{Op: "wt-write", WID: "w1", Stream: "s1", Binary: write("close")})
	events.next(t, "ack")
	bridge.Handle(BridgeMessage{Op: "wt-fin", WID: "w1", Stream: "s1"})
	for {
		event := <-events
		if event["event"] == "closed" {
			if event["code"] != 7 || event["reason"] != "bye" {
				t.Fatalf("closed = %v", event)
			}
			return
		}
	}
}

func TestWebTransportBridgeRefusesAWrongCertificateHash(t *testing.T) {
	server := startEchoServer(t)
	bridge, events := newBridge(t)
	other := sha256.Sum256([]byte("another certificate"))
	bridge.Handle(BridgeMessage{Op: "wt-open", WID: "w1", URL: server.url, Hashes: []string{base64.StdEncoding.EncodeToString(other[:])}})
	event := events.next(t, "failed")
	if !strings.Contains(event["error"].(string), "serverCertificateHashes") {
		t.Fatalf("error = %v", event["error"])
	}
}

func TestWebTransportBridgeNeedsPinnedHashesAndPrivateAddresses(t *testing.T) {
	events := make(eventLog, 8)
	bridge := &WebTransportBridge{Deliver: func(event map[string]any) { events <- event }}
	defer bridge.Shutdown()
	hash := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, test := range []struct {
		message BridgeMessage
		want    string
	}{
		{BridgeMessage{Op: "wt-open", WID: "a", URL: "https://192.168.1.81:9011/dynapp-remote"}, "needs serverCertificateHashes"},
		{BridgeMessage{Op: "wt-open", WID: "b", URL: "http://192.168.1.81:9011/", Hashes: []string{hash}}, "https URL"},
		{BridgeMessage{Op: "wt-open", WID: "c", URL: "https://192.168.1.81:9011/", Hashes: []string{"short"}}, "SHA-256"},
		{BridgeMessage{Op: "wt-open", WID: "d", URL: "https://8.8.8.8:9011/", Hashes: []string{hash}}, "not on a private network"},
		{BridgeMessage{Op: "wt-open", WID: "e", URL: "https://127.0.0.1:9011/", Hashes: []string{hash}}, "not on a private network"},
	} {
		bridge.Handle(test.message)
		event := events.next(t, "failed")
		if !strings.Contains(event["error"].(string), test.want) {
			t.Fatalf("%s: error = %v, want %q", test.message.URL, event["error"], test.want)
		}
	}
}

func TestPrivateNetworkAddress(t *testing.T) {
	for address, want := range map[string]bool{
		"192.168.1.81": true, "10.2.6.211": true, "172.20.0.4": true, "169.254.3.4": true,
		"100.101.102.103": true, "fd7a:115c:a1e0::1": true, "fe80::1": true,
		"127.0.0.1": false, "::1": false, "8.8.8.8": false, "100.128.0.1": false, "0.0.0.0": false,
	} {
		if got := PrivateNetworkAddress(net.ParseIP(address)); got != want {
			t.Errorf("%s: got %v, want %v", address, got, want)
		}
	}
}
