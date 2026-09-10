package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

type bridgeSet struct {
	mu     sync.Mutex
	items  map[string]*bridgeRecord
	active *atomic.Int64
}
type bridgeRecord struct {
	connection net.Conn
	carrier    protocolSocket
}

func newBridgeSet(counters ...*atomic.Int64) *bridgeSet {
	var active *atomic.Int64
	if len(counters) > 0 {
		active = counters[0]
	}
	return &bridgeSet{items: map[string]*bridgeRecord{}, active: active}
}
func (set *bridgeSet) put(id string, connection net.Conn, carrier protocolSocket) {
	set.mu.Lock()
	set.items[id] = &bridgeRecord{connection: connection, carrier: carrier}
	set.mu.Unlock()
	if set.active != nil {
		set.active.Add(1)
	}
}
func (set *bridgeSet) get(id string) net.Conn {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil {
		return record.connection
	}
	return nil
}
func (set *bridgeSet) carrier(id string) protocolSocket {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil {
		return record.carrier
	}
	return nil
}
func (set *bridgeSet) bind(id string, carrier protocolSocket) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil {
		record.carrier = carrier
		return true
	}
	return false
}
func (set *bridgeSet) take(id string) net.Conn {
	set.mu.Lock()
	defer set.mu.Unlock()
	record := set.items[id]
	delete(set.items, id)
	if record == nil {
		return nil
	}
	if set.active != nil {
		set.active.Add(-1)
	}
	return record.connection
}
func (set *bridgeSet) closeAll() {
	set.mu.Lock()
	connections := make([]net.Conn, 0, len(set.items))
	count := int64(len(set.items))
	for _, record := range set.items {
		connections = append(connections, record.connection)
	}
	set.items = map[string]*bridgeRecord{}
	set.mu.Unlock()
	if set.active != nil && count > 0 {
		set.active.Add(-count)
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (s *Server) handleNetwork(socket protocolSocket, ctx context.Context, bridges *bridgeSet, request message, body []byte) {
	switch request.Action {
	case "open":
		if request.ID == "" || (request.ProtocolName != "tcp" && request.ProtocolName != "udp" && request.ProtocolName != "rdp") || strings.TrimSpace(request.Target.Host) == "" || request.Target.Port < 1 || request.Target.Port > 65535 || request.Target.LocalPort < 0 || request.Target.LocalPort > 65535 || (request.Target.LocalPort != 0 && request.ProtocolName != "udp") {
			send(socket, ctx, map[string]any{"type": "net-error", "id": request.ID, "error": "Remote network request is invalid"})
			return
		}
		release, allowed := s.lockBridgeOpen()
		if !allowed {
			send(socket, ctx, map[string]any{"type": "net-error", "id": request.ID, "error": "The agent is preparing an update"})
			return
		}
		defer release()
		var connection net.Conn
		var err error
		result := map[string]any{}
		if request.ProtocolName == "rdp" {
			token := randomBridgeToken()
			connection, err = openRDPBridge(request.Target.Host, request.Target.Port, token)
			result["authToken"] = token
		} else if request.ProtocolName == "udp" {
			connection, err = dialUDPBridge(request.Target.Host, request.Target.Port, request.Target.LocalPort)
		} else {
			connection, err = net.DialTimeout("tcp", net.JoinHostPort(request.Target.Host, strconv.Itoa(request.Target.Port)), 15*time.Second)
		}
		if err != nil {
			send(socket, ctx, map[string]any{"type": "net-error", "id": request.ID, "error": err.Error()})
			return
		}
		bridgeID := fmt.Sprintf("%s_%d", request.ProtocolName, s.bridgeCounter.Add(1))
		if request.ProtocolName == "udp" {
			log.Printf("UDP bridge %s:%d: local %s", request.Target.Host, request.Target.Port, connection.LocalAddr())
		}
		result["bridgeId"] = bridgeID
		bridges.put(bridgeID, connection, socket)
		if !send(socket, ctx, map[string]any{"type": "net-result", "id": request.ID, "result": result}) {
			_ = bridges.take(bridgeID).Close()
			return
		}
		go s.forwardTCPBridge(socket, bridges, bridgeID, connection)
	case "data":
		connection := bridges.get(request.BridgeID)
		if connection == nil || len(body) == 0 {
			return
		}
		if _, err := connection.Write(body); err != nil {
			_ = bridges.take(request.BridgeID).Close()
		}
	case "close":
		if connection := bridges.take(request.BridgeID); connection != nil {
			_ = connection.Close()
		}
	default:
		send(socket, ctx, map[string]any{"type": "net-error", "id": request.ID, "error": "Remote network request is invalid"})
	}
}

func dialUDPBridge(host string, port, localPort int) (net.Conn, error) {
	remote, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	connection, err := net.DialUDP("udp", &net.UDPAddr{Port: localPort}, remote)
	if err != nil {
		return nil, err
	}
	// Apple High Performance video can arrive in short multi-megabyte bursts.
	// The default OS UDP receive buffer is too small for a bridge that forwards
	// each datagram through a reliable browser channel.
	_ = connection.SetReadBuffer(16 * 1024 * 1024)
	return connection, nil
}

func transientUDPReadError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.Errno(10054)) || // Windows WSAECONNRESET
		errors.Is(err, syscall.Errno(10061)) // Windows WSAECONNREFUSED
}

func (s *Server) forwardTCPBridge(socket protocolSocket, bridges *bridgeSet, bridgeID string, connection net.Conn) {
	buffer := make([]byte, 32*1024)
	total := 0
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			total += count
			carrier := bridges.carrier(bridgeID)
			if carrier == nil {
				return
			}
			sendBinary(carrier, context.Background(), map[string]any{"type": "net-data", "bridgeId": bridgeID}, buffer[:count])
		}
		if err != nil {
			if strings.HasPrefix(bridgeID, "udp_") && transientUDPReadError(err) {
				log.Printf("UDP bridge %s -> %s ignored transient read error: %v", connection.LocalAddr(), connection.RemoteAddr(), err)
				continue
			}
			if strings.HasPrefix(bridgeID, "udp_") {
				log.Printf("UDP bridge %s -> %s ended after %d bytes: %v", connection.LocalAddr(), connection.RemoteAddr(), total, err)
			}
			if bridges.take(bridgeID) != nil {
				reason := "TCP bridge closed"
				if strings.HasPrefix(bridgeID, "udp_") {
					reason = "UDP bridge closed"
				}
				send(socket, context.Background(), map[string]any{"type": "net-close", "bridgeId": bridgeID, "code": 1000, "reason": reason})
			}
			return
		}
	}
}

func (s *Server) handleReliableChannel(ctx context.Context, channel protocolSocket, bridges *bridgeSet) {
	defer channel.Close(websocket.StatusNormalClosure, "")
	kind, data, err := channel.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		return
	}
	var bind message
	if json.Unmarshal(data, &bind) != nil || bind.Type != "channel" || bind.Action != "bind" || protocolNumber(bind.Protocol) != ProtocolVersion || !bridges.bind(bind.BridgeID, channel) {
		return
	}
	for {
		kind, data, err = channel.Read(ctx)
		if err != nil {
			return
		}
		var request message
		var body []byte
		if kind == websocket.MessageBinary {
			request, body, err = decodeBinaryFrame(data)
		} else {
			err = json.Unmarshal(data, &request)
		}
		if err != nil {
			return
		}
		if request.Type == "net-data" && request.BridgeID == bind.BridgeID && len(body) > 0 {
			if connection := bridges.get(bind.BridgeID); connection != nil {
				_, _ = connection.Write(body)
			}
		} else if request.Type == "net-close" {
			if connection := bridges.take(bind.BridgeID); connection != nil {
				_ = connection.Close()
			}
			return
		}
	}
}
