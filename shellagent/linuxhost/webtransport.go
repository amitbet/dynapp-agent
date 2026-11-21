package linuxhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// WebKitGTK has no WebTransport, so a Linux app page could reach another
// computer's agent only through the relay, even on the same LAN. The host
// stands in for the browser: the page gets a window.WebTransport whose
// sessions and streams the host opens with webtransport-go, over the same
// bridge as the agent socket (docs/native-host.md, "Linux WebTransport").
//
// It is narrower than a browser's WebTransport on purpose: every session
// needs a pinned certificate hash, and only private-network addresses
// (LAN, link-local, CGNAT/Tailscale) are dialed. That covers the agents'
// LAN listeners and nothing on the public Internet.

//go:embed webtransport_polyfill.js
var webTransportPolyfill string

const (
	wtMaxSessions     = 32
	wtMaxStreams      = 512
	wtReadChunk       = 256 * 1024
	wtDialTimeout     = 15 * time.Second
	wtMaxCloseReason  = 1024
	wtMaxIDLength     = 64
	wtMaxURLLength    = 2048
	wtMaxHashes       = 8
	wtWriteQueueDepth = 64
)

// WebTransportBridge serves the page's WebTransport stand-in.
type WebTransportBridge struct {
	// Origin is the app page's origin, sent as the CONNECT Origin header
	// exactly as a browser would.
	Origin string
	// Deliver sends one event to the page.
	Deliver func(map[string]any)
	// AllowAddress decides which resolved addresses may be dialed. Nil
	// means private-network addresses only.
	AllowAddress func(net.IP) bool
	// Resolve looks up a host name. Nil uses the system resolver.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)

	mu       sync.Mutex
	sessions map[string]*wtSession
	closed   bool
}

type wtSession struct {
	id      string
	cancel  context.CancelFunc
	mu      sync.Mutex
	session *webtransport.Session
	streams map[string]*wtStream
	ended   bool
	// pending holds operations that arrived before the session was ready.
	pending []func(*webtransport.Session)
}

type wtStream struct {
	id     string
	stream *webtransport.Stream
	writes chan wtWrite
	reads  chan struct{}
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	finSent  bool
	readDone bool
	stopped  bool
}

func (s *wtStream) opened() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream != nil && !s.stopped
}

type wtWrite struct {
	data []byte
	fin  bool
}

// Handle runs one wt-* page message. It never blocks on the network.
func (b *WebTransportBridge) Handle(message BridgeMessage) {
	if message.Op != "wt-reset" && (message.WID == "" || len(message.WID) > wtMaxIDLength || len(message.Stream) > wtMaxIDLength) {
		return
	}
	switch message.Op {
	case "wt-reset":
		// A new page document: drop the previous page's sessions.
		b.CloseAll()
	case "wt-open":
		b.open(message)
	case "wt-close":
		b.closeSession(message.WID, message.Code, message.Reason)
	case "wt-stream":
		b.withSession(message.WID, func(s *wtSession, session *webtransport.Session) { b.openStream(s, session, message.Stream) })
	case "wt-write":
		data, err := base64.StdEncoding.DecodeString(message.Binary)
		if err != nil {
			b.Deliver(map[string]any{"wid": message.WID, "stream": message.Stream, "event": "write-error", "error": "invalid stream data"})
			return
		}
		b.queueWrite(message.WID, message.Stream, wtWrite{data: data})
	case "wt-fin":
		b.queueWrite(message.WID, message.Stream, wtWrite{fin: true})
	case "wt-read":
		if stream := b.stream(message.WID, message.Stream); stream != nil {
			select {
			case stream.reads <- struct{}{}:
			case <-stream.done:
			default:
				// One read is already queued; the page waits for it.
			}
		}
	case "wt-abort":
		if stream := b.stream(message.WID, message.Stream); stream != nil {
			b.dropStream(message.WID, stream, false)
		}
	}
}

// CloseAll ends every session, for a page reload or the window closing.
func (b *WebTransportBridge) CloseAll() {
	b.mu.Lock()
	sessions := b.sessions
	b.sessions = map[string]*wtSession{}
	b.mu.Unlock()
	for _, s := range sessions {
		s.end(0, "")
	}
}

// Shutdown ends every session and refuses new ones.
func (b *WebTransportBridge) Shutdown() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.CloseAll()
}

func (b *WebTransportBridge) open(message BridgeMessage) {
	fail := func(text string) {
		b.Deliver(map[string]any{"wid": message.WID, "event": "failed", "error": text})
	}
	target, hashes, err := b.validateOpen(message)
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &wtSession{id: message.WID, cancel: cancel, streams: map[string]*wtStream{}}
	b.mu.Lock()
	if b.sessions == nil {
		b.sessions = map[string]*wtSession{}
	}
	_, exists := b.sessions[message.WID]
	switch {
	case b.closed:
		err = errors.New("the app window is closing")
	case exists:
		err = errors.New("duplicate WebTransport session id")
	case len(b.sessions) >= wtMaxSessions:
		err = errors.New("too many WebTransport sessions")
	default:
		b.sessions[message.WID] = s
	}
	b.mu.Unlock()
	if err != nil {
		cancel()
		fail(err.Error())
		return
	}
	go b.dial(ctx, s, target, hashes)
}

func (b *WebTransportBridge) validateOpen(message BridgeMessage) (*url.URL, [][]byte, error) {
	if len(message.URL) > wtMaxURLLength {
		return nil, nil, errors.New("WebTransport URL is too long")
	}
	target, err := url.Parse(message.URL)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil {
		return nil, nil, errors.New("WebTransport needs an https URL")
	}
	if len(message.Hashes) == 0 {
		// A browser would also accept a publicly trusted certificate; this
		// stand-in exists for pinned LAN agents only.
		return nil, nil, errors.New("WebTransport in the Linux app host needs serverCertificateHashes")
	}
	if len(message.Hashes) > wtMaxHashes {
		return nil, nil, errors.New("too many serverCertificateHashes")
	}
	hashes := make([][]byte, 0, len(message.Hashes))
	for _, encoded := range message.Hashes {
		hash, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(hash) != sha256.Size {
			return nil, nil, errors.New("serverCertificateHashes must be SHA-256 values")
		}
		hashes = append(hashes, hash)
	}
	return target, hashes, nil
}

func (b *WebTransportBridge) dial(ctx context.Context, s *wtSession, target *url.URL, hashes [][]byte) {
	session, err := b.connect(ctx, target, hashes)
	if err != nil {
		b.forget(s)
		s.cancel()
		b.Deliver(map[string]any{"wid": s.id, "event": "failed", "error": err.Error()})
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		_ = session.CloseWithError(0, "")
		return
	}
	s.session = session
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	b.Deliver(map[string]any{"wid": s.id, "event": "ready"})
	for _, run := range pending {
		run(session)
	}
	go b.watch(s, session)
}

func (b *WebTransportBridge) connect(ctx context.Context, target *url.URL, hashes [][]byte) (*webtransport.Session, error) {
	dialCtx, cancel := context.WithTimeout(ctx, wtDialTimeout)
	defer cancel()
	host := target.Hostname()
	port := target.Port()
	if port == "" {
		port = "443"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, errors.New("WebTransport URL has an invalid port")
	}
	addresses, err := b.resolve(dialCtx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	allow := b.AllowAddress
	if allow == nil {
		allow = PrivateNetworkAddress
	}
	var permitted []net.IP
	for _, address := range addresses {
		if allow(address) {
			permitted = append(permitted, address)
		}
	}
	if len(permitted) == 0 {
		return nil, fmt.Errorf("%s is not on a private network; the Linux app host dials LAN agents only", host)
	}
	tlsConfig := &tls.Config{
		ServerName: host,
		// The certificate is self-signed and pinned instead: accept it only
		// when its SHA-256 matches a hash the page supplied, as Chromium does
		// for serverCertificateHashes.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("server sent no certificate")
			}
			sum := sha256.Sum256(raw[0])
			for _, hash := range hashes {
				if bytes.Equal(hash, sum[:]) {
					return nil
				}
			}
			return errors.New("server certificate does not match serverCertificateHashes")
		},
	}
	var lastErr error
	for _, address := range permitted {
		dialer := webtransport.Dialer{
			TLSClientConfig: tlsConfig,
			QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, KeepAlivePeriod: 15 * time.Second},
			DialAddr: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
				// Dial the address that passed the check, never a fresh
				// lookup of the host name.
				return quic.DialAddrEarly(ctx, net.JoinHostPort(address.String(), port), tlsCfg, cfg)
			},
		}
		header := http.Header{}
		if b.Origin != "" {
			header.Set("Origin", b.Origin)
		}
		response, session, err := dialer.Dial(dialCtx, target.String(), header)
		if err == nil {
			return session, nil
		}
		if response != nil && response.StatusCode != 0 {
			err = fmt.Errorf("WebTransport handshake refused: HTTP %d", response.StatusCode)
		}
		lastErr = err
	}
	return nil, lastErr
}

func (b *WebTransportBridge) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	if b.Resolve != nil {
		return b.Resolve(ctx, host)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, address.IP)
	}
	return ips, nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// PrivateNetworkAddress reports whether ip is on a LAN, link-local, or
// carrier-grade NAT (Tailscale) network. Loopback is excluded: this
// machine's agent is reached through the host's own agent socket.
func PrivateNetworkAddress(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

// watch reports the session's end to the page.
func (b *WebTransportBridge) watch(s *wtSession, session *webtransport.Session) {
	<-session.Context().Done()
	code, reason := 0, ""
	// AcceptStream returns the session's close error once it has ended.
	if _, err := session.AcceptStream(context.Background()); err != nil {
		var sessionErr *webtransport.SessionError
		if errors.As(err, &sessionErr) {
			code, reason = int(sessionErr.ErrorCode), sessionErr.Message
		}
	}
	b.forget(s)
	s.end(0, "")
	b.Deliver(map[string]any{"wid": s.id, "event": "closed", "code": code, "reason": reason})
}

func (b *WebTransportBridge) forget(s *wtSession) {
	b.mu.Lock()
	if b.sessions[s.id] == s {
		delete(b.sessions, s.id)
	}
	b.mu.Unlock()
}

func (b *WebTransportBridge) session(id string) *wtSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

// withSession runs op once the session is ready, in order with other ops.
func (b *WebTransportBridge) withSession(id string, op func(*wtSession, *webtransport.Session)) {
	s := b.session(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if s.session == nil {
		s.pending = append(s.pending, func(session *webtransport.Session) { op(s, session) })
		s.mu.Unlock()
		return
	}
	session := s.session
	s.mu.Unlock()
	op(s, session)
}

func (b *WebTransportBridge) closeSession(id string, code int, reason string) {
	s := b.session(id)
	if s == nil {
		return
	}
	if len(reason) > wtMaxCloseReason {
		reason = reason[:wtMaxCloseReason]
	}
	if code < 0 || code > 0xffffffff {
		code = 0
	}
	s.end(code, reason)
}

func (s *wtSession) end(code int, reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	session := s.session
	streams := s.streams
	s.streams = map[string]*wtStream{}
	s.pending = nil
	s.mu.Unlock()
	for _, stream := range streams {
		stream.stop()
	}
	if session != nil {
		go func() { _ = session.CloseWithError(webtransport.SessionErrorCode(code), reason) }()
	}
	s.cancel()
}

func (b *WebTransportBridge) openStream(s *wtSession, session *webtransport.Session, id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	_, exists := s.streams[id]
	full := len(s.streams) >= wtMaxStreams
	s.mu.Unlock()
	if exists || full {
		b.Deliver(map[string]any{"wid": s.id, "stream": id, "event": "stream-error", "error": "cannot open another stream"})
		return
	}
	// Reserve the id so writes queued before the open completes find it.
	stream := &wtStream{id: id, writes: make(chan wtWrite, wtWriteQueueDepth), reads: make(chan struct{}, 1), done: make(chan struct{})}
	s.mu.Lock()
	s.streams[id] = stream
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(session.Context(), wtDialTimeout)
		opened, err := session.OpenStreamSync(ctx)
		cancel()
		if err != nil {
			b.dropStream(s.id, stream, false)
			b.Deliver(map[string]any{"wid": s.id, "stream": id, "event": "stream-error", "error": err.Error()})
			return
		}
		stream.mu.Lock()
		stopped := stream.stopped
		if !stopped {
			stream.stream = opened
		}
		stream.mu.Unlock()
		if stopped {
			opened.CancelRead(0)
			opened.CancelWrite(0)
			return
		}
		b.Deliver(map[string]any{"wid": s.id, "stream": id, "event": "stream-ready"})
		go b.writeLoop(s.id, stream)
		go b.readLoop(s.id, stream)
	}()
}

func (b *WebTransportBridge) stream(sessionID, streamID string) *wtStream {
	s := b.session(sessionID)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stream := s.streams[streamID]
	if stream == nil || !stream.opened() {
		return nil
	}
	return stream
}

func (b *WebTransportBridge) queueWrite(sessionID, streamID string, write wtWrite) {
	s := b.session(sessionID)
	if s == nil {
		return
	}
	s.mu.Lock()
	stream := s.streams[streamID]
	s.mu.Unlock()
	if stream == nil {
		b.Deliver(map[string]any{"wid": sessionID, "stream": streamID, "event": "write-error", "error": "stream is closed"})
		return
	}
	select {
	case stream.writes <- write:
	case <-stream.done:
	default:
		// The page waits for each write's ack, so a full queue means a
		// misbehaving page rather than ordinary buffering.
		b.Deliver(map[string]any{"wid": sessionID, "stream": streamID, "event": "write-error", "error": "too many unacknowledged writes"})
	}
}

func (b *WebTransportBridge) writeLoop(sessionID string, stream *wtStream) {
	for {
		select {
		case <-stream.done:
			return
		case write := <-stream.writes:
			if write.fin {
				err := stream.stream.Close()
				b.Deliver(writeResult(sessionID, stream.id, "fin", err))
				if err != nil {
					b.dropStream(sessionID, stream, false)
					return
				}
				b.finish(sessionID, stream, true, false)
				continue
			}
			_, err := stream.stream.Write(write.data)
			b.Deliver(writeResult(sessionID, stream.id, "ack", err))
			if err != nil {
				b.dropStream(sessionID, stream, false)
				return
			}
		}
	}
}

func writeResult(sessionID, streamID, event string, err error) map[string]any {
	if err != nil {
		return map[string]any{"wid": sessionID, "stream": streamID, "event": "write-error", "error": err.Error()}
	}
	return map[string]any{"wid": sessionID, "stream": streamID, "event": event}
}

// readLoop reads one chunk for each request from the page, so the page's
// ReadableStream sets the pace.
func (b *WebTransportBridge) readLoop(sessionID string, stream *wtStream) {
	buffer := make([]byte, wtReadChunk)
	for {
		select {
		case <-stream.done:
			return
		case <-stream.reads:
		}
		n, err := stream.stream.Read(buffer)
		if n > 0 {
			b.Deliver(map[string]any{"wid": sessionID, "stream": stream.id, "event": "data", "binary": base64.StdEncoding.EncodeToString(buffer[:n])})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				b.Deliver(map[string]any{"wid": sessionID, "stream": stream.id, "event": "end"})
				b.finish(sessionID, stream, false, true)
			} else {
				b.Deliver(map[string]any{"wid": sessionID, "stream": stream.id, "event": "read-error", "error": err.Error()})
				b.dropStream(sessionID, stream, false)
			}
			return
		}
		if n == 0 {
			// Nothing delivered for this request; ask the stream again.
			select {
			case stream.reads <- struct{}{}:
			default:
			}
		}
	}
}

// finish records a finished direction and forgets the stream once both
// are done. Its FIN may still be in flight, so it is not reset.
func (b *WebTransportBridge) finish(sessionID string, stream *wtStream, fin, read bool) {
	stream.mu.Lock()
	stream.finSent = stream.finSent || fin
	stream.readDone = stream.readDone || read
	both := stream.finSent && stream.readDone
	stream.mu.Unlock()
	if both {
		b.dropStream(sessionID, stream, true)
	}
}

// dropStream forgets a stream. graceful leaves a closed stream alone;
// otherwise both directions are reset.
func (b *WebTransportBridge) dropStream(sessionID string, stream *wtStream, graceful bool) {
	if s := b.session(sessionID); s != nil {
		s.mu.Lock()
		if s.streams[stream.id] == stream {
			delete(s.streams, stream.id)
		}
		s.mu.Unlock()
	}
	if graceful {
		stream.once.Do(func() {
			stream.mu.Lock()
			stream.stopped = true
			stream.mu.Unlock()
			close(stream.done)
		})
		return
	}
	stream.stop()
}

func (s *wtStream) stop() {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		opened := s.stream
		s.mu.Unlock()
		close(s.done)
		if opened != nil {
			opened.CancelRead(0)
			opened.CancelWrite(0)
		}
	})
}
