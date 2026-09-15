package shellagent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/rdp"
	"nhooyr.io/websocket"
)

type bridgeSet struct {
	mu     sync.Mutex
	items  map[string]*bridgeRecord
	active *atomic.Int64
}

var bridgeDatagramCounter atomic.Uint32

type bridgeRecord struct {
	connection net.Conn
	carrier    protocolSocket
	protocol   string
	mode       string
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
	protocol, _, _ := strings.Cut(id, "_")
	set.items[id] = &bridgeRecord{connection: connection, carrier: carrier, protocol: protocol}
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
func (set *bridgeSet) bind(id string, carrier protocolSocket, mode string) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil {
		record.carrier = carrier
		record.mode = mode
		return true
	}
	return false
}
func (set *bridgeSet) bindDatagrams(id string) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil && record.protocol == "udp" {
		record.mode = "datagram"
		return true
	}
	return false
}
func (set *bridgeSet) delivery(id string) (protocolSocket, string) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if record := set.items[id]; record != nil {
		return record.carrier, record.mode
	}
	return nil, ""
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
			connection, err = rdp.OpenBridge(request.Target.Host, request.Target.Port, token)
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
	if strings.HasPrefix(bridgeID, "udp_") {
		buffer = make([]byte, 65535)
	}
	total := 0
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			total += count
			carrier, mode := bridges.delivery(bridgeID)
			if carrier == nil {
				return
			}
			if mode == "raw" {
				if raw, ok := carrier.(interface {
					WriteRaw(context.Context, []byte) error
				}); !ok || raw.WriteRaw(context.Background(), buffer[:count]) != nil {
					_ = bridges.take(bridgeID).Close()
					return
				}
			} else if mode == "datagram" {
				if datagrams, ok := carrier.(interface{ SendDatagram([]byte) error }); ok {
					for _, datagram := range encodeBridgeDatagrams(bridgeID, buffer[:count]) {
						if datagrams.SendDatagram(datagram) != nil {
							// QUIC datagrams may be dropped. A local send error drops
							// this UDP packet without tearing down its socket.
							break
						}
					}
				}
			} else {
				sendBinary(carrier, context.Background(), map[string]any{"type": "net-data", "bridgeId": bridgeID}, buffer[:count])
			}
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

func (s *Server) handleReliableChannel(ctx context.Context, channel protocolSocket, bridges *bridgeSet, authentication socketAuthentication) {
	defer channel.Close(websocket.StatusNormalClosure, "")
	kind, data, err := channel.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		return
	}
	var bind message
	if json.Unmarshal(data, &bind) != nil || bind.Type != "channel" || bind.Action != "bind" || protocolNumber(bind.Protocol) != ProtocolVersion {
		return
	}
	if bind.Kind == "fs" {
		s.handleFilesystemChannel(ctx, channel, bind, authentication)
		return
	}
	mode := ""
	if bind.Mode == "raw" {
		if _, ok := channel.(interface {
			ReadRaw(context.Context, []byte) (int, error)
		}); !ok {
			return
		}
		mode = "raw"
	}
	if !bridges.bind(bind.BridgeID, channel, mode) {
		return
	}
	defer func() {
		if connection := bridges.take(bind.BridgeID); connection != nil {
			_ = connection.Close()
		}
	}()
	if mode == "raw" {
		raw := channel.(interface {
			ReadRaw(context.Context, []byte) (int, error)
		})
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := raw.ReadRaw(ctx, buffer)
			if count > 0 {
				if connection := bridges.get(bind.BridgeID); connection != nil {
					if writeErr := writeStreamBytes(connection, buffer[:count]); writeErr != nil {
						_ = connection.Close()
						return
					}
				}
			}
			if readErr != nil {
				return
			}
		}
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

func writeStreamBytes(connection net.Conn, body []byte) error {
	for len(body) > 0 {
		count, err := connection.Write(body)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrUnexpectedEOF
		}
		body = body[count:]
	}
	return nil
}

const bridgeDatagramBytes = 1000
const bridgeDatagramHeaderBytes = 14
const maxPendingBridgeDatagrams = 256

func encodeBridgeDatagrams(bridgeID string, body []byte) [][]byte {
	id := []byte(bridgeID)
	fragmentBytes := bridgeDatagramBytes - bridgeDatagramHeaderBytes - len(id)
	if len(id) == 0 || len(id) > 0xffff || len(body) > 65535 || fragmentBytes < 1 {
		return nil
	}
	fragmentCount := max(1, (len(body)+fragmentBytes-1)/fragmentBytes)
	if fragmentCount > 0xffff {
		return nil
	}
	messageID := bridgeDatagramCounter.Add(1)
	frames := make([][]byte, fragmentCount)
	for fragmentIndex := range fragmentCount {
		start := fragmentIndex * fragmentBytes
		end := min(len(body), start+fragmentBytes)
		frame := make([]byte, bridgeDatagramHeaderBytes+len(id)+end-start)
		copy(frame[:4], "DGD1")
		binary.BigEndian.PutUint32(frame[4:8], messageID)
		binary.BigEndian.PutUint16(frame[8:10], uint16(fragmentIndex))
		binary.BigEndian.PutUint16(frame[10:12], uint16(fragmentCount))
		binary.BigEndian.PutUint16(frame[12:14], uint16(len(id)))
		copy(frame[bridgeDatagramHeaderBytes:], id)
		copy(frame[bridgeDatagramHeaderBytes+len(id):], body[start:end])
		frames[fragmentIndex] = frame
	}
	return frames
}

type bridgeDatagramFragment struct {
	bridgeID                     string
	messageID                    uint32
	fragmentIndex, fragmentCount int
	body                         []byte
}

func decodeBridgeDatagram(frame []byte) (bridgeDatagramFragment, bool) {
	if len(frame) < bridgeDatagramHeaderBytes || len(frame) > bridgeDatagramBytes || string(frame[:4]) != "DGD1" {
		return bridgeDatagramFragment{}, false
	}
	fragmentIndex := int(binary.BigEndian.Uint16(frame[8:10]))
	fragmentCount := int(binary.BigEndian.Uint16(frame[10:12]))
	idLength := int(binary.BigEndian.Uint16(frame[12:14]))
	if fragmentCount == 0 || fragmentCount > 128 || fragmentIndex >= fragmentCount || idLength == 0 || bridgeDatagramHeaderBytes+idLength > len(frame) {
		return bridgeDatagramFragment{}, false
	}
	return bridgeDatagramFragment{
		bridgeID:      string(frame[bridgeDatagramHeaderBytes : bridgeDatagramHeaderBytes+idLength]),
		messageID:     binary.BigEndian.Uint32(frame[4:8]),
		fragmentIndex: fragmentIndex,
		fragmentCount: fragmentCount,
		body:          append([]byte(nil), frame[bridgeDatagramHeaderBytes+idLength:]...),
	}, true
}

type bridgeDatagramAssembly struct {
	fragments [][]byte
	updatedAt time.Time
}

func decodeCompleteBridgeDatagram(pending map[string]*bridgeDatagramAssembly, frame []byte) (string, []byte, bool) {
	fragment, ok := decodeBridgeDatagram(frame)
	if !ok {
		return "", nil, false
	}
	if fragment.fragmentCount == 1 {
		return fragment.bridgeID, fragment.body, true
	}
	now := time.Now()
	for key, assembly := range pending {
		if now.Sub(assembly.updatedAt) > 2*time.Second {
			delete(pending, key)
		}
	}
	key := fmt.Sprintf("%s:%d", fragment.bridgeID, fragment.messageID)
	assembly := pending[key]
	if assembly == nil || len(assembly.fragments) != fragment.fragmentCount {
		if assembly == nil && len(pending) >= maxPendingBridgeDatagrams {
			var oldestKey string
			var oldestTime time.Time
			for candidateKey, candidate := range pending {
				if oldestKey == "" || candidate.updatedAt.Before(oldestTime) {
					oldestKey, oldestTime = candidateKey, candidate.updatedAt
				}
			}
			delete(pending, oldestKey)
		}
		assembly = &bridgeDatagramAssembly{fragments: make([][]byte, fragment.fragmentCount)}
		pending[key] = assembly
	}
	assembly.fragments[fragment.fragmentIndex] = fragment.body
	assembly.updatedAt = now
	total := 0
	for _, part := range assembly.fragments {
		if part == nil {
			return "", nil, false
		}
		total += len(part)
		if total > 65535 {
			delete(pending, key)
			return "", nil, false
		}
	}
	delete(pending, key)
	body := make([]byte, 0, total)
	for _, part := range assembly.fragments {
		body = append(body, part...)
	}
	return fragment.bridgeID, body, true
}

func (s *Server) handleBridgeDatagrams(ctx context.Context, receiver interface {
	ReceiveDatagram(context.Context) ([]byte, error)
}, bridges *bridgeSet) {
	pending := map[string]*bridgeDatagramAssembly{}
	for {
		frame, err := receiver.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		bridgeID, body, ok := decodeCompleteBridgeDatagram(pending, frame)
		if !ok {
			continue
		}
		_, mode := bridges.delivery(bridgeID)
		if mode != "datagram" {
			continue
		}
		if connection := bridges.get(bridgeID); connection != nil {
			_, _ = connection.Write(body)
		}
	}
}
