package shellagent

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
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"nhooyr.io/websocket"
)

const lanCertificateLifetime = 13 * 24 * time.Hour

type lanCertificate struct {
	TLS      tls.Certificate
	Hash     string
	NotAfter time.Time
}
type storedLANCertificate struct {
	CertificatePEM, PrivateKeyPEM, Hash string
	NotAfter                            int64
}

func (s *Server) loadOrCreateLANCertificate() (lanCertificate, error) {
	root := s.StateDir
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return lanCertificate{}, err
		}
	}
	path := filepath.Join(root, "lan-certificate.json")
	if data, err := os.ReadFile(path); err == nil {
		var stored storedLANCertificate
		if json.Unmarshal(data, &stored) == nil && time.UnixMilli(stored.NotAfter).After(time.Now().Add(24*time.Hour)) {
			pair, err := tls.X509KeyPair([]byte(stored.CertificatePEM), []byte(stored.PrivateKeyPEM))
			if err == nil {
				cert, parseErr := x509.ParseCertificate(pair.Certificate[0])
				if parseErr == nil && !cert.IsCA {
					pair.Leaf = cert
					return lanCertificate{TLS: pair, Hash: stored.Hash, NotAfter: cert.NotAfter}, nil
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return lanCertificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return lanCertificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "DynApp LAN WebTransport"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(lanCertificateLifetime), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return lanCertificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return lanCertificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	hashBytes := sha256.Sum256(der)
	hash := base64.RawURLEncoding.EncodeToString(hashBytes[:])
	stored := storedLANCertificate{CertificatePEM: string(certPEM), PrivateKeyPEM: string(keyPEM), Hash: hash, NotAfter: template.NotAfter.UnixMilli()}
	data, _ := json.MarshalIndent(stored, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return lanCertificate{}, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return lanCertificate{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return lanCertificate{}, err
	}
	pair.Leaf, _ = x509.ParseCertificate(der)
	return lanCertificate{TLS: pair, Hash: hash, NotAfter: template.NotAfter}, nil
}

func privateOrLoopbackHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	ip := net.ParseIP(trimmed)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

func (s *Server) startLAN(config Config) (func() error, error) {
	certificate, err := s.loadOrCreateLANCertificate()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	server := &webtransport.Server{H3: &http3.Server{Addr: config.LANAddress, TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{certificate.TLS}}), Handler: mux}, CheckOrigin: func(request *http.Request) bool {
		// The same allowlist as the loopback WebSocket: hosted app origins,
		// stored identity origins, and loopback development origins.
		return s.originAllowed(request.Header.Get("Origin"))
	}}
	webtransport.ConfigureHTTP3Server(server.H3)
	mux.HandleFunc(RemotePath, func(w http.ResponseWriter, r *http.Request) {
		session, err := server.Upgrade(w, r)
		if err != nil {
			return
		}
		go s.serveLANSession(session, config, r.Header.Get("Origin"))
	})
	udpAddress, err := net.ResolveUDPAddr("udp", config.LANAddress)
	if err != nil {
		return nil, err
	}
	packet, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, err
	}
	go func() { _ = server.Serve(packet) }()
	return func() error { _ = packet.Close(); return server.Close() }, nil
}

func (s *Server) serveLANSession(session *webtransport.Session, config Config, requestOrigin string) {
	ctx := session.Context()
	first, err := session.AcceptStream(ctx)
	if err != nil {
		_ = session.CloseWithError(1, "A protocol stream is required")
		return
	}
	main := &webTransportRecordSocket{session: session, stream: first, closeSession: true}
	// Direct local access is authenticated before a normal remote-protocol
	// hello. This keeps bearer pairing tokens out of the browser-facing path.
	parsedOrigin, ok := parseOrigin(requestOrigin)
	if !ok {
		_ = main.Close(closeIdentityRejected, "Browser identity rejected")
		return
	}
	requestOrigin = strings.ToLower(parsedOrigin.Scheme + "://" + parsedOrigin.Host)
	directAuth, outcome := s.authenticateDirectLocal(ctx, main, requestOrigin)
	switch outcome {
	case directLocalOK:
	case directLocalProbe, directLocalAborted:
		// A probe closes itself after it receives the challenge. Closing the
		// server side of its only QUIC stream races that close in Chromium and
		// surfaces as RESET_STREAM. Leave it open until the client finishes it.
		return
	case directLocalApprovalRequired:
		_ = main.Close(closeApprovalRequired, "Open this app from Dyner to approve access")
		return
	case directLocalDeclined:
		_ = main.Close(closePairingDeclined, "Pairing declined")
		return
	default:
		_ = main.Close(closeIdentityRejected, "Browser identity rejected")
		return
	}
	channels := make(chan protocolSocket, 8)
	go func() {
		defer close(channels)
		for {
			stream, err := session.AcceptStream(ctx)
			if err != nil {
				return
			}
			channels <- &webTransportRecordSocket{session: session, stream: stream}
		}
	}()
	s.serveAuthenticatedSocket(ctx, main, func(message) socketAuthentication { return directAuth }, true, channels)
}

type webTransportRecordSocket struct {
	session      *webtransport.Session
	stream       *webtransport.Stream
	closeSession bool
	writeMu      sync.Mutex
}

func (s *webTransportRecordSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	header := make([]byte, 4)
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.stream.SetReadDeadline(deadline)
	}
	if _, err := io.ReadFull(s.stream, header); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header)
	if size > 21*1024*1024 {
		return 0, nil, errors.New("remote transport record is too large")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(s.stream, payload); err != nil {
		return 0, nil, err
	}
	kind := websocket.MessageText
	if len(payload) >= 4 && string(payload[:4]) == "DFB1" {
		kind = websocket.MessageBinary
	}
	return kind, payload, nil
}
func (s *webTransportRecordSocket) Write(ctx context.Context, _ websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.stream.SetWriteDeadline(deadline)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := s.stream.Write(header); err != nil {
		return err
	}
	_, err := s.stream.Write(payload)
	return err
}
func (s *webTransportRecordSocket) Close(code websocket.StatusCode, reason string) error {
	if s.closeSession {
		// Closing this stream first can make Chromium report RESET_STREAM before
		// it receives the session status code. The session close includes the
		// meaningful approval or authentication reason for the browser.
		return s.session.CloseWithError(webtransport.SessionErrorCode(code), reason)
	}
	return s.stream.Close()
}

func (s *Server) LANCertificateHash() (map[string]string, error) {
	certificate, err := s.loadOrCreateLANCertificate()
	if err != nil {
		return nil, err
	}
	return map[string]string{"algorithm": "sha-256", "value": certificate.Hash}, nil
}
