package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// The inbox is a hibernated WebSocket to the environment's EnvInbox Durable
// Object in the relay worker. Dyner pushes events into it so the agent reacts
// to a new ICE session or relay request at once instead of on its next
// browser-identities sync, which stays as the fallback (docs/mesh-network.md,
// section 1).

const (
	remoteStateRelaxedSyncInterval = 30 * time.Second
	inboxPingInterval              = 30 * time.Second
	// inboxCloseRevoked is the worker's close code after a revoke event.
	inboxCloseRevoked websocket.StatusCode = 4003
)

type inboxTicket struct {
	URL       string `json:"url"`
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
}

type inboxEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

var errInboxRevoked = errors.New("inbox revoked")

func fetchInboxTicket(ctx context.Context, client *http.Client, config Config) (inboxTicket, error) {
	if err := config.Validate(); err != nil {
		return inboxTicket{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base, err := url.Parse(config.DynerBaseURL)
	if err != nil {
		return inboxTicket{}, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" + url.PathEscape(config.EnvironmentID) + "/inbox-ticket"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return inboxTicket{}, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return inboxTicket{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return inboxTicket{}, fmt.Errorf("Dyner inbox ticket failed: HTTP %d", response.StatusCode)
	}
	var ticket inboxTicket
	if err := json.NewDecoder(response.Body).Decode(&ticket); err != nil {
		return inboxTicket{}, err
	}
	endpoint, err := url.Parse(ticket.URL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "wss" && endpoint.Scheme != "ws") || ticket.Ticket == "" {
		return inboxTicket{}, errors.New("Dyner returned an invalid inbox ticket")
	}
	return ticket, nil
}

func (ticket inboxTicket) dialURL() string {
	endpoint, _ := url.Parse(ticket.URL)
	query := endpoint.Query()
	query.Set("ticket", ticket.Ticket)
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func inboxKey(config Config) string {
	return config.DynerBaseURL + "\x00" + config.EnvironmentID + "\x00" + config.DeviceCredential
}

func (s *Server) reconcileInbox() {
	config, want := s.meshConfig()
	key := inboxKey(config)
	s.meshMu.Lock()
	if s.inboxCancel != nil && (!want || s.inboxKey != key) {
		s.inboxCancel()
		s.inboxCancel = nil
		s.inboxKey = ""
	}
	if !want {
		s.inboxRevoked = false
	}
	if !want || s.inboxCancel != nil || s.inboxRevoked {
		s.meshMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.inboxCancel = cancel
	s.inboxKey = key
	s.meshMu.Unlock()
	go s.runInbox(ctx, cancel, config)
}

func (s *Server) stopInbox() {
	s.meshMu.Lock()
	cancel := s.inboxCancel
	s.inboxCancel = nil
	s.inboxKey = ""
	s.meshMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// noteRemoteSyncSucceeded lets a revoked inbox reconnect: Dyner still
// accepts this device, so the revoke was not for this credential any more.
func (s *Server) noteRemoteSyncSucceeded() {
	s.meshMu.Lock()
	s.inboxRevoked = false
	s.meshMu.Unlock()
}

func inboxBackoff(current time.Duration) time.Duration {
	// Uniform in [d/2, 3d/2) so agents that lost the worker together do not
	// reconnect together.
	return current/2 + time.Duration(rand.Int64N(int64(current)+1))
}

func (s *Server) runInbox(ctx context.Context, cancel context.CancelFunc, config Config) {
	defer cancel()
	timing := s.timing()
	backoff := timing.backoffMin
	for ctx.Err() == nil {
		connected, err := s.inboxSession(ctx, config)
		if errors.Is(err, errInboxRevoked) {
			log.Printf("DynApp Shell agent: inbox revoked; waiting for a successful sync")
			s.meshMu.Lock()
			if s.inboxKey == inboxKey(config) {
				s.inboxCancel = nil
				s.inboxKey = ""
				s.inboxRevoked = true
			}
			s.meshMu.Unlock()
			return
		}
		if connected {
			backoff = timing.backoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(inboxBackoff(backoff), timing.backoffMax)):
		}
		backoff = min(backoff*2, timing.backoffMax)
	}
}

// inboxSession runs one connection. connected reports whether the socket
// opened, which resets the reconnect backoff.
func (s *Server) inboxSession(ctx context.Context, config Config) (connected bool, err error) {
	ticket, err := fetchInboxTicket(ctx, nil, config)
	if err != nil {
		return false, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dialCtx, ticket.dialURL(), nil)
	cancelDial()
	if err != nil {
		return false, err
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "")
	s.inboxConnected.Store(true)
	defer func() {
		s.inboxConnected.Store(false)
		// Back to the fast sync, and catch up on anything missed while the
		// socket was going down.
		s.requestRemoteSync()
	}()
	timing := s.timing()
	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())
	go func() {
		ticker := time.NewTicker(timing.ping)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
			}
			// The worker auto-answers "pong" without waking; silence for two
			// intervals means the path is dead even if TCP has not noticed.
			if time.Since(time.Unix(0, lastSeen.Load())) > 2*timing.ping+5*time.Second {
				_ = conn.Close(websocket.StatusGoingAway, "keepalive timeout")
				return
			}
			writeCtx, cancelWrite := context.WithTimeout(sessionCtx, 10*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, []byte("ping"))
			cancelWrite()
			if err != nil {
				_ = conn.Close(websocket.StatusGoingAway, "keepalive failed")
				return
			}
		}
	}()
	for {
		kind, data, err := conn.Read(sessionCtx)
		if err != nil {
			if websocket.CloseStatus(err) == inboxCloseRevoked {
				return true, errInboxRevoked
			}
			return true, err
		}
		lastSeen.Store(time.Now().UnixNano())
		if kind != websocket.MessageText || string(data) == "pong" {
			continue
		}
		var event inboxEvent
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		if event.Type == "revoke" {
			_ = conn.Close(websocket.StatusNormalClosure, "revoked")
			return true, errInboxRevoked
		}
		s.handleInboxEvent(config, event)
	}
}

func (s *Server) handleInboxEvent(config Config, event inboxEvent) {
	switch event.Type {
	case "ice-session":
		if strings.TrimSpace(event.ID) == "" {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			session, err := fetchICESession(ctx, nil, config, event.ID)
			cancel()
			if err != nil {
				logICEError(event.ID, err)
				return
			}
			if session.ID == "" {
				session.ID = event.ID
			}
			// The same path as the periodic sync: answering is idempotent per
			// session id, so a sync that sees this offer too does nothing.
			s.startPendingICESessions(context.Background(), config, []iceSession{session})
		}()
	case "relay-request":
		go s.applyRelayRequest(true)
	case "identities-changed":
		s.requestRemoteSync()
	}
}
