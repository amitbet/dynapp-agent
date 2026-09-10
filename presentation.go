package shellagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Each authenticated app socket owns a helper. Closing the private channel
// removes every native resource, including after revocation or agent failure.
type presentationBridge struct {
	conn    net.Conn
	mu      sync.Mutex
	replies chan presentationReply
	done    chan struct{}
	stop    func()
	once    sync.Once
}
type presentationCommand struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Method  string `json:"method"`
	Args    []any  `json:"args"`
}
type presentationReply struct {
	ID      string `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	Service string `json:"service,omitempty"`
	Event   any    `json:"event,omitempty"`
}

func (b *presentationBridge) close() {
	b.once.Do(func() { b.conn.Close(); time.AfterFunc(time.Second, b.stop) })
}
func (s *Server) startPresentation(socket protocolSocket, ctx context.Context) (*presentationBridge, error) {
	if !presentationSupported() {
		return nil, errors.New("native tray and global shortcuts are unavailable in this agent build")
	}
	return s.connectPresentation(socket, ctx, launchPresentation)
}

func (s *Server) connectPresentation(socket protocolSocket, ctx context.Context, launch func(string, []string, string) (func(), error)) (*presentationBridge, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	stopCancellation := context.AfterFunc(ctx, func() { listener.Close() })
	defer stopCancellation()
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	stop, err := launch(exe, []string{"presentation-helper", listener.Addr().String()}, token)
	if err != nil {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			stop()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	listener.SetDeadline(deadline)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return nil, fmt.Errorf("native helper connection: %w", err)
		}
		conn.SetReadDeadline(minPresentationDeadline(deadline, time.Now().Add(time.Second)))
		reader := bufio.NewReaderSize(conn, 4096)
		lineBytes, err := reader.ReadSlice('\n')
		line := string(lineBytes)
		if err != nil || len(line) != len(token)+1 || subtle.ConstantTimeCompare([]byte(line[:len(line)-1]), []byte(token)) != 1 {
			conn.Close()
			continue
		}
		conn.SetReadDeadline(time.Time{})
		b := &presentationBridge{conn: conn, replies: make(chan presentationReply, 1), done: make(chan struct{}), stop: stop}
		accepted = true
		s.presentationMu.Lock()
		if s.presentationClosed {
			s.presentationMu.Unlock()
			b.close()
			return nil, errors.New("agent is shutting down")
		}
		if s.presentations == nil {
			s.presentations = map[*presentationBridge]struct{}{}
		}
		s.presentations[b] = struct{}{}
		s.presentationMu.Unlock()
		go func() {
			defer func() { s.presentationMu.Lock(); delete(s.presentations, b); s.presentationMu.Unlock() }()
			defer close(b.done)
			defer b.close()
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 4096), 16*1024*1024)
			for scanner.Scan() {
				var reply presentationReply
				if json.Unmarshal(scanner.Bytes(), &reply) != nil {
					return
				}
				if reply.Event != nil {
					send(socket, ctx, map[string]any{"type": "rpc-event", "service": reply.Service, "event": reply.Event})
				} else {
					select {
					case b.replies <- reply:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return b, nil
	}
}

func minPresentationDeadline(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
func (b *presentationBridge) call(ctx context.Context, request message) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(b.conn).Encode(presentationCommand{ID: request.ID, Service: request.Service, Method: request.Method, Args: request.Args}); err != nil {
		b.close()
		return nil, err
	}
	timeout := 10 * time.Second
	if request.Service == "screen" {
		timeout = 60 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply := <-b.replies:
		if reply.ID != request.ID {
			b.close()
			return nil, errors.New("native helper response id mismatch")
		}
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return reply.Result, nil
	case <-b.done:
		return nil, errors.New("native helper disconnected; reconnect the app to register again")
	case <-ctx.Done():
		b.close()
		return nil, ctx.Err()
	case <-timer.C:
		b.close()
		return nil, errors.New("native helper timed out")
	}
}

func (s *Server) closePresentations() {
	s.presentationMu.Lock()
	defer s.presentationMu.Unlock()
	s.presentationClosed = true
	for bridge := range s.presentations {
		bridge.close()
	}
}

// RunPresentationHelper is private to the agent. Its random, single-use
// loopback channel accepts only presentation commands, never privileged RPCs.
func RunPresentationHelper(address, token string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || len(token) != 64 {
		return errors.New("invalid presentation channel")
	}
	conn, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = fmt.Fprintln(conn, token); err != nil {
		return err
	}
	var mu sync.Mutex
	emit := func(reply presentationReply) {
		mu.Lock()
		defer mu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if json.NewEncoder(conn).Encode(reply) != nil {
			conn.Close()
		}
	}
	commands := make(chan presentationCommand)
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var command presentationCommand
			if json.Unmarshal(scanner.Bytes(), &command) != nil {
				break
			}
			commands <- command
		}
	}()
	return runNativePresentation(commands, emit)
}
