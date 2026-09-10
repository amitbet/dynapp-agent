package rdp

import (
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestRDPTLSConfigCapsCredSSPTransportAtTLS12(t *testing.T) {
	config := rdpTLSConfig("rdp.example")
	if config.ServerName != "rdp.example" {
		t.Fatalf("ServerName = %q", config.ServerName)
	}
	if config.MinVersion != tls.VersionTLS12 || config.MaxVersion != tls.VersionTLS12 {
		t.Fatalf("TLS range = %x-%x, want TLS 1.2 only", config.MinVersion, config.MaxVersion)
	}
}

func TestLegacyRDPTLSConfigAllowsOnlyRSAWithAESCBC(t *testing.T) {
	config := legacyRDPTLSConfig("legacy-rdp.example")
	if config.ServerName != "legacy-rdp.example" {
		t.Fatalf("ServerName = %q", config.ServerName)
	}
	if config.MinVersion != tls.VersionTLS10 || config.MaxVersion != tls.VersionTLS12 {
		t.Fatalf("TLS range = %x-%x, want TLS 1.0 through 1.2", config.MinVersion, config.MaxVersion)
	}
	want := []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA}
	if len(config.CipherSuites) != len(want) {
		t.Fatalf("cipher suites = %v", config.CipherSuites)
	}
	for index := range want {
		if config.CipherSuites[index] != want[index] {
			t.Fatalf("cipher suite %d = %x, want %x", index, config.CipherSuites[index], want[index])
		}
	}
	if !isRDPProtocolVersionError(errors.New("remote error: tls: protocol version not supported")) {
		t.Fatal("protocol-version alert should enable the compatibility retry")
	}
	if !isRDPProtocolVersionError(errors.New("tls: server selected unsupported protocol version 301")) {
		t.Fatal("a legacy server-selected version should enable the compatibility retry")
	}
	if isRDPProtocolVersionError(errors.New("remote error: tls: handshake failure")) {
		t.Fatal("unrelated TLS failures must not enable the compatibility retry")
	}
}

func TestPreferTLSOnlyRDPNegotiationRemovesCredSSP(t *testing.T) {
	hybrid, err := hex.DecodeString("030000130ee00000000000010008000b000000")
	if err != nil {
		t.Fatal(err)
	}
	got := preferTLSOnlyRDPNegotiation(hybrid)
	if encoded := hex.EncodeToString(got); encoded != "030000130ee000000000000100080001000000" {
		t.Fatalf("TLS-only X.224 request = %s", encoded)
	}
	if encoded := hex.EncodeToString(hybrid); encoded != "030000130ee00000000000010008000b000000" {
		t.Fatalf("source X.224 request was mutated: %s", encoded)
	}
}

func TestRDPBridgeCompatibilityTarget(t *testing.T) {
	target := os.Getenv("DYNAPP_RDP_TEST_TARGET")
	if target == "" {
		t.Skip("DYNAPP_RDP_TEST_TARGET is not set")
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	token := "compatibility-test-token"
	bridge, err := openRDPBridge(host, port, token)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	requestRoot := derExplicitValue(0, derEncode(0x02, []byte{0x0d, 0x3e}))
	requestRoot = append(requestRoot, derExplicitValue(2, derEncode(0x0c, []byte(target)))...)
	requestRoot = append(requestRoot, derExplicitValue(3, derEncode(0x0c, []byte(token)))...)
	x224 := []byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x08, 0x00, 0x0b, 0x00, 0x00, 0x00}
	requestRoot = append(requestRoot, derExplicitValue(6, derEncode(0x04, x224))...)
	if _, err = bridge.Write(derEncode(0x30, requestRoot)); err != nil {
		t.Fatal(err)
	}
	if err = bridge.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := readDER(bridge, 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	_, root, _, err := derTLV(response, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := derExplicit(root, 0xa7); !ok {
		t.Fatalf("RDCleanPath response does not contain a certificate chain: %x", response)
	}
}

var cleanPathReference = []byte{0x30, 0x32, 0xA0, 0x04, 0x02, 0x02, 0x0D, 0x3E, 0xA2, 0x0D, 0x0C, 0x0B, 0x64, 0x65, 0x73, 0x74, 0x69, 0x6E, 0x61, 0x74, 0x69, 0x6F, 0x6E, 0xA3, 0x0C, 0x0C, 0x0A, 0x70, 0x72, 0x6F, 0x78, 0x79, 0x20, 0x61, 0x75, 0x74, 0x68, 0xA5, 0x05, 0x0C, 0x03, 0x50, 0x43, 0x42, 0xA6, 0x06, 0x04, 0x04, 0xDE, 0xAD, 0xBE, 0xFF}

func TestRdcCleanPathMatchesElectronReference(t *testing.T) {
	request, err := parseRdcCleanPathRequest(cleanPathReference)
	if err != nil {
		t.Fatal(err)
	}
	if request.Destination != "destination" || request.ProxyAuth != "proxy auth" || hex.EncodeToString(request.X224) != "deadbeff" {
		t.Fatalf("request = %#v", request)
	}
	certificate := []byte{0xde, 0xad, 0xbe, 0xff}
	encoded := encodeRdcCleanPathResponse("192.168.7.95", certificate, [][]byte{certificate, certificate, certificate})
	if got := hex.EncodeToString(encoded); got != "3034a00402020d3ea6060404deadbeffa71430120404deadbeff0404deadbeff0404deadbeffa90e0c0c3139322e3136382e372e3935" {
		t.Fatalf("response = %s", got)
	}
}

func TestRdcCleanPathErrorEncoding(t *testing.T) {
	encoded := encodeRdcCleanPathError(10013)
	if got := hex.EncodeToString(encoded); got != "3015a00402020d3ea10d300ba003020101a2040202271d" {
		t.Fatalf("error response = %s", got)
	}
	if got := rdpWSAError(syscall.EPERM); got != 10013 {
		t.Fatalf("permission error = %d", got)
	}
	if got := rdpWSAError(errors.New("other")); got != 0 {
		t.Fatalf("generic error = %d", got)
	}
}

func TestRDPBridgeReturnsProtocolErrorWhenTargetRefusesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	token := "test-token"
	bridge, err := openRDPBridge("127.0.0.1", port, token)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	requestRoot := derExplicitValue(0, derEncode(0x02, []byte{0x0d, 0x3e}))
	requestRoot = append(requestRoot, derExplicitValue(2, derEncode(0x0c, []byte("127.0.0.1")))...)
	requestRoot = append(requestRoot, derExplicitValue(3, derEncode(0x0c, []byte(token)))...)
	requestRoot = append(requestRoot, derExplicitValue(6, derEncode(0x04, []byte{3, 0, 0, 4}))...)
	if _, err := bridge.Write(derEncode(0x30, requestRoot)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if len(response) == 0 {
		t.Fatal("bridge closed without an RDCleanPath error response")
	}
	if got := hex.EncodeToString(response); got != "3015a00402020d3ea10d300ba003020101a2040202274d" {
		t.Fatalf("refused response = %s", got)
	}
}
