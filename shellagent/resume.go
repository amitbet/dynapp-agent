package shellagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Resumable sessions (docs/mesh-network.md, "Phase 2a: resilient sessions").
// A session's processes, bridges and pending replies outlive the carrier the
// browser opened it on, so a phone that changes networks or a relay socket
// that is replaced resumes on another carrier without losing a frame.

const (
	closeSessionMoved   websocket.StatusCode = 4409
	closeSessionExpired websocket.StatusCode = 4410

	defaultResumeGrace = 60 * time.Second
	// resumeReplayLimit bounds unacknowledged frames per session. A session
	// past it stops being resumable rather than holding unbounded memory.
	resumeReplayLimit = 16 << 20
	resumeAckFrames   = 64
	resumeAckBytes    = 1 << 20
	resumeAckDelay    = time.Second
)

var errSessionEnded = errors.New("session ended")

// errDatagramsUnavailable tells a datagram-bound bridge that the session's
// current carrier has no datagram channel, so the packet goes on the control
// channel instead.
var errDatagramsUnavailable = errors.New("carrier has no datagram channel")

type helloSession struct {
	Resumable bool   `json:"resumable"`
	Resume    string `json:"resume"`
	Token     string `json:"token"`
	Received  uint64 `json:"received"`
}

// parseHelloSession reads the hello's `session` member. Other frames may use
// the same key for unrelated values, which is why message keeps it raw.
func parseHelloSession(raw json.RawMessage) *helloSession {
	if len(raw) == 0 {
		return nil
	}
	var parsed helloSession
	if json.Unmarshal(raw, &parsed) != nil {
		return nil
	}
	return &parsed
}

// resumableCarrierFeatures removes what is bound to one carrier. Substreams
// and their bytes are not replayed, so bridges and file chunks stay on the
// control channel, which is.
func resumableCarrierFeatures(carrier carrierCapabilities) carrierCapabilities {
	carrier.reliableStreams = false
	carrier.rawBridgeStreams = false
	carrier.filesystemStreams = false
	return carrier
}

type replayFrame struct {
	number uint64
	kind   websocket.MessageType
	data   []byte
}

type sessionCarrier struct {
	socket     protocolSocket
	ctx        context.Context
	cancel     context.CancelFunc
	features   carrierCapabilities
	generation uint64
}

// sessionSocket is the protocolSocket a resumable session's handlers write
// to. It numbers and buffers every frame and forwards it to whichever carrier
// is attached; while detached, frames only go to the replay buffer.
type sessionSocket struct {
	server  *Server
	id      string
	token   string
	keyID   string
	storeID string
	grace   time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	// capabilities is the enforced grant, reported again on resume.
	capabilities []string
	// scoped is the share scope of the socket that opened the session. The
	// session keeps serving with that socket's authentication; a resume must
	// come from an equally scoped carrier.
	scoped bool

	incoming  chan dataChannelMessage
	datagrams chan []byte

	// deliverMu orders an old carrier's last delivered frame before the
	// count a resume reply reports. writeMu orders frames on a carrier
	// against the replay of a resume. Lock order: deliverMu, writeMu, mu.
	deliverMu sync.Mutex
	writeMu   sync.Mutex

	mu            sync.Mutex
	carrier       *sessionCarrier
	generation    uint64
	sent          uint64
	replay        []replayFrame
	replayBytes   int
	overflowed    bool
	received      uint64
	unackedFrames int
	unackedBytes  int
	ackTimer      *time.Timer
	graceTimer    *time.Timer
	ended         bool
}

func randomSessionValue(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (s *Server) resumeGrace() time.Duration {
	if s.resumeGraceOverride > 0 {
		return s.resumeGraceOverride
	}
	return defaultResumeGrace
}

// newSessionSocket registers a resumable session. Its context descends from
// parent's values but not its cancellation: the carrier that opened the
// session may die long before the session does.
func (s *Server) newSessionSocket(parent context.Context, auth socketAuthentication) (*sessionSocket, error) {
	id, err := randomSessionValue(16)
	if err != nil {
		return nil, err
	}
	token, err := randomSessionValue(32)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	session := &sessionSocket{
		server: s, id: id, token: token, keyID: auth.keyID, storeID: auth.storeID,
		grace: s.resumeGrace(), ctx: ctx, cancel: cancel,
		capabilities: auth.capabilities,
		scoped:       auth.scoped,
		incoming:     make(chan dataChannelMessage, 256),
		datagrams:    make(chan []byte, 256),
	}
	s.resumeMu.Lock()
	if s.resumeSessions == nil {
		s.resumeSessions = map[string]*sessionSocket{}
	}
	s.resumeSessions[id] = session
	s.resumeMu.Unlock()
	return session, nil
}

func (s *Server) lookupSessionSocket(id string) *sessionSocket {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	return s.resumeSessions[id]
}

func (s *Server) endResumableSessions() {
	s.resumeMu.Lock()
	sessions := make([]*sessionSocket, 0, len(s.resumeSessions))
	for _, session := range s.resumeSessions {
		sessions = append(sessions, session)
	}
	s.resumeMu.Unlock()
	for _, session := range sessions {
		session.end(websocket.StatusGoingAway, "Agent shutting down")
	}
}

// authorize checks a resume hello against the session in constant time for
// the token, so a guessed id reveals nothing about it.
func (session *sessionSocket) authorize(token string, auth socketAuthentication) bool {
	if subtle.ConstantTimeCompare([]byte(session.token), []byte(token)) != 1 {
		return false
	}
	return session.keyID == auth.keyID && session.storeID == auth.storeID && session.scoped == auth.scoped
}

// attach makes socket the session's carrier. received is the number of
// frames the browser holds (0 for a new session); reply builds the
// uncounted hello reply from the number of browser frames this side holds.
// On success the previous carrier, if any, is closed with 4409.
func (session *sessionSocket) attach(socket protocolSocket, ctx context.Context, features carrierCapabilities, received uint64, reply func(received uint64) map[string]any) error {
	session.deliverMu.Lock()
	session.writeMu.Lock()
	session.mu.Lock()
	first := session.sent - uint64(len(session.replay))
	if session.ended || session.overflowed || received > session.sent || received < first {
		session.mu.Unlock()
		session.writeMu.Unlock()
		session.deliverMu.Unlock()
		return errSessionEnded
	}
	session.generation++
	generation := session.generation
	previous := session.carrier
	session.carrier = nil
	if previous != nil {
		previous.cancel()
	}
	session.trimReplayLocked(received)
	replay := append([]replayFrame(nil), session.replay...)
	browserFrames := session.received
	session.unackedFrames, session.unackedBytes = 0, 0
	if session.ackTimer != nil {
		session.ackTimer.Stop()
		session.ackTimer = nil
	}
	session.mu.Unlock()

	err := writeJSONFrame(socket, ctx, reply(browserFrames))
	for index := 0; err == nil && index < len(replay); index++ {
		err = socket.Write(ctx, replay[index].kind, replay[index].data)
	}
	carrierCtx, cancel := context.WithCancel(ctx)
	attached := &sessionCarrier{socket: socket, ctx: carrierCtx, cancel: cancel, features: features, generation: generation}
	session.mu.Lock()
	if err == nil {
		session.carrier = attached
		if session.graceTimer != nil {
			session.graceTimer.Stop()
			session.graceTimer = nil
		}
	} else {
		cancel()
		session.startGraceLocked()
	}
	session.mu.Unlock()
	session.writeMu.Unlock()
	session.deliverMu.Unlock()
	if err != nil {
		return err
	}
	go session.pump(attached)
	if receiver, ok := socket.(interface {
		ReceiveDatagram(context.Context) ([]byte, error)
	}); ok && features.datagrams {
		go session.pumpDatagrams(attached, receiver)
	}
	if previous != nil {
		closeCarrier(previous.socket, context.Background(), closeSessionMoved, "Session moved")
	}
	return nil
}

// trimReplayLocked drops frames the browser has acknowledged.
func (session *sessionSocket) trimReplayLocked(acknowledged uint64) {
	drop := 0
	for drop < len(session.replay) && session.replay[drop].number <= acknowledged {
		session.replayBytes -= len(session.replay[drop].data)
		drop++
	}
	if drop > 0 {
		session.replay = append(session.replay[:0:0], session.replay[drop:]...)
	}
}

// pump moves one carrier's frames into the session until the carrier dies or
// a resume moves the session elsewhere.
func (session *sessionSocket) pump(carrier *sessionCarrier) {
	for {
		kind, data, err := carrier.socket.Read(carrier.ctx)
		if err != nil {
			session.detach(carrier.generation)
			return
		}
		if acknowledged, ok := parseSessionAck(kind, data); ok {
			session.mu.Lock()
			if carrier.generation == session.generation {
				session.trimReplayLocked(acknowledged)
			}
			session.mu.Unlock()
			continue
		}
		session.deliverMu.Lock()
		session.mu.Lock()
		if carrier.generation != session.generation || session.ended {
			session.mu.Unlock()
			session.deliverMu.Unlock()
			return
		}
		session.received++
		session.unackedFrames++
		session.unackedBytes += len(data)
		ackNow := session.unackedFrames >= resumeAckFrames || session.unackedBytes >= resumeAckBytes
		if !ackNow && session.ackTimer == nil {
			session.ackTimer = time.AfterFunc(resumeAckDelay, session.sendAck)
		}
		session.mu.Unlock()
		select {
		case session.incoming <- dataChannelMessage{kind: kind, data: data}:
		case <-session.ctx.Done():
		}
		session.deliverMu.Unlock()
		if ackNow {
			session.sendAck()
		}
	}
}

func (session *sessionSocket) pumpDatagrams(carrier *sessionCarrier, receiver interface {
	ReceiveDatagram(context.Context) ([]byte, error)
}) {
	for {
		payload, err := receiver.ReceiveDatagram(carrier.ctx)
		if err != nil {
			return
		}
		select {
		case session.datagrams <- payload:
		default:
			// UDP semantics: a slow consumer loses packets, not the session.
		}
	}
}

func parseSessionAck(kind websocket.MessageType, data []byte) (uint64, bool) {
	if kind != websocket.MessageText || len(data) > 256 || !bytes.Contains(data, []byte(`"session-ack"`)) {
		return 0, false
	}
	var ack struct {
		Type     string `json:"type"`
		Received uint64 `json:"received"`
	}
	if json.Unmarshal(data, &ack) != nil || ack.Type != "session-ack" {
		return 0, false
	}
	return ack.Received, true
}

// sendAck acknowledges every browser frame received so far. Acks are not
// counted or buffered: a lost one is superseded by the next or by the count
// in a resume reply.
func (session *sessionSocket) sendAck() {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	session.mu.Lock()
	if session.ackTimer != nil {
		session.ackTimer.Stop()
		session.ackTimer = nil
	}
	carrier := session.carrier
	received := session.received
	pending := session.unackedFrames > 0
	session.unackedFrames, session.unackedBytes = 0, 0
	session.mu.Unlock()
	if carrier == nil || !pending {
		return
	}
	if writeJSONFrame(carrier.socket, carrier.ctx, map[string]any{"type": "session-ack", "received": received}) != nil {
		session.detach(carrier.generation)
	}
}

// detach drops a dead carrier. The session waits for a resume for its grace
// period, unless it is no longer resumable.
func (session *sessionSocket) detach(generation uint64) {
	session.mu.Lock()
	if session.carrier == nil || session.carrier.generation != generation || session.ended {
		session.mu.Unlock()
		return
	}
	session.carrier.cancel()
	session.carrier = nil
	session.generation++
	overflowed := session.overflowed
	if !overflowed {
		session.startGraceLocked()
	}
	session.mu.Unlock()
	if overflowed {
		session.end(websocket.StatusGoingAway, "Session ended")
	}
}

func (session *sessionSocket) startGraceLocked() {
	if session.graceTimer != nil || session.ended {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(session.grace, func() {
		session.mu.Lock()
		expired := session.graceTimer == timer && session.carrier == nil
		session.mu.Unlock()
		if expired {
			session.end(closeSessionExpired, "Session expired")
		}
	})
	session.graceTimer = timer
}

// end stops the session exactly as a closed socket used to: the session loop
// returns and releases processes, bridges and agents.
func (session *sessionSocket) end(code websocket.StatusCode, reason string) {
	session.mu.Lock()
	if session.ended {
		session.mu.Unlock()
		return
	}
	session.ended = true
	carrier := session.carrier
	session.carrier = nil
	if session.graceTimer != nil {
		session.graceTimer.Stop()
		session.graceTimer = nil
	}
	if session.ackTimer != nil {
		session.ackTimer.Stop()
		session.ackTimer = nil
	}
	session.replay, session.replayBytes = nil, 0
	session.mu.Unlock()
	session.cancel()
	session.server.resumeMu.Lock()
	if session.server.resumeSessions[session.id] == session {
		delete(session.server.resumeSessions, session.id)
	}
	session.server.resumeMu.Unlock()
	if carrier != nil {
		carrier.cancel()
		closeCarrier(carrier.socket, context.Background(), code, reason)
	}
}

func (session *sessionSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case message := <-session.incoming:
		return message.kind, message.data, nil
	case <-session.ctx.Done():
		return 0, nil, errSessionEnded
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

// Write numbers and buffers a frame, then sends it on the current carrier.
// A carrier that fails detaches; the frame stays buffered for the resume, so
// the caller sees success.
func (session *sessionSocket) Write(_ context.Context, kind websocket.MessageType, data []byte) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	session.mu.Lock()
	if session.ended {
		session.mu.Unlock()
		return errSessionEnded
	}
	session.sent++
	if !session.overflowed {
		if session.replayBytes+len(data) > resumeReplayLimit {
			session.overflowed = true
			session.replay, session.replayBytes = nil, 0
		} else {
			session.replay = append(session.replay, replayFrame{number: session.sent, kind: kind, data: append([]byte(nil), data...)})
			session.replayBytes += len(data)
		}
	}
	carrier := session.carrier
	overflowed := session.overflowed
	session.mu.Unlock()
	if carrier == nil {
		if overflowed {
			session.end(closeSessionExpired, "Session expired")
			return errSessionEnded
		}
		return nil
	}
	if err := carrier.socket.Write(carrier.ctx, kind, data); err != nil {
		session.detach(carrier.generation)
		if overflowed {
			return err
		}
	}
	return nil
}

func (session *sessionSocket) Close(code websocket.StatusCode, reason string) error {
	session.end(code, reason)
	return nil
}

func (session *sessionSocket) SendDatagram(payload []byte) error {
	session.mu.Lock()
	carrier := session.carrier
	session.mu.Unlock()
	if carrier == nil || !carrier.features.datagrams {
		return errDatagramsUnavailable
	}
	sender, ok := carrier.socket.(interface{ SendDatagram([]byte) error })
	if !ok {
		return errDatagramsUnavailable
	}
	return sender.SendDatagram(payload)
}

func (session *sessionSocket) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-session.datagrams:
		return payload, nil
	case <-session.ctx.Done():
		return nil, errSessionEnded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func writeJSONFrame(socket protocolSocket, ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return socket.Write(ctx, websocket.MessageText, data)
}

// closeCarrier ends one carrier. A data channel cannot carry a WebSocket
// close code, so a session move or expiry first arrives as the `error` frame
// a browser already treats as a failed connection; other closes on it stay
// silent as before. A relay virtual socket cannot be closed at all and always
// reports its close that way (relayVirtualSocket.Close).
func closeCarrier(socket protocolSocket, ctx context.Context, code websocket.StatusCode, reason string) {
	if _, ok := socket.(*webRTCChannelSocket); ok && (code == closeSessionMoved || code == closeSessionExpired) {
		_ = writeJSONFrame(socket, ctx, map[string]any{"type": "error", "error": reason, "code": int(code)})
	}
	_ = socket.Close(code, reason)
}

// resumeSession moves an existing session onto a newly opened carrier, or
// closes that carrier with 4410 when the session cannot be resumed.
func (s *Server) resumeSession(ctx context.Context, socket protocolSocket, request *helloSession, protocol int, auth socketAuthentication, carrier carrierCapabilities) {
	session := s.lookupSessionSocket(request.Resume)
	if session == nil || protocol != ProtocolVersion || !carrier.resumable || auth.keyID == "" || !session.authorize(request.Token, auth) {
		closeCarrier(socket, ctx, closeSessionExpired, "Session expired")
		return
	}
	features := resumableCarrierFeatures(carrier)
	err := session.attach(socket, ctx, features, request.Received, func(received uint64) map[string]any {
		reply := map[string]any{
			"type": "hello", "ok": true, "resumed": true, "protocol": ProtocolVersion,
			"serverId":     "go-shell-agent",
			"session":      map[string]any{"id": session.id, "received": received},
			"capabilities": session.capabilities,
			"features":     sessionFeatures(features),
			"environment":  auth.environment,
		}
		if carrier.portMapping != nil {
			reply["transport"] = map[string]any{"portMapping": carrier.portMapping}
		}
		return reply
	})
	if err != nil {
		closeCarrier(socket, ctx, closeSessionExpired, "Session expired")
	}
}

func sessionFeatures(carrier carrierCapabilities) map[string]bool {
	features := protocolFeatures(ProtocolVersion, carrier)
	features["resume"] = true
	return features
}
