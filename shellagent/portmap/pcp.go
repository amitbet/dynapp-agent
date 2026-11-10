package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// PCP (RFC 6887) MAP request and response layout.
const (
	pcpServerPort     = 5351
	pcpVersion        = 2
	pcpOpcodeMap      = 1
	pcpResponseBit    = 0x80
	pcpMapPacketSize  = 60
	pcpProtocolUDP    = 17
	pcpResultSuccess  = 0
	pcpUnsuppVersion  = 1
	pcpInitialTimeout = 250 * time.Millisecond
)

var pcpResultNames = map[byte]string{
	1: "UNSUPP_VERSION", 2: "NOT_AUTHORIZED", 3: "MALFORMED_REQUEST", 4: "UNSUPP_OPCODE",
	5: "UNSUPP_OPTION", 6: "MALFORMED_OPTION", 7: "NETWORK_FAILURE", 8: "NO_RESOURCES",
	9: "UNSUPP_PROTOCOL", 10: "USER_EX_QUOTA", 11: "CANNOT_PROVIDE_EXTERNAL",
	12: "ADDRESS_MISMATCH", 13: "EXCESSIVE_REMOTE_PEERS",
}

type pcpMapper struct {
	gateway netip.AddrPort
	once    sync.Once
	// nonce identifies this client's mapping; renewals and the delete must
	// repeat it (RFC 6887 section 11.1).
	nonce [12]byte
}

func (m *pcpMapper) name() string { return ProtocolPCP }

func (m *pcpMapper) add(ctx context.Context, internalPort, externalPort int, lifetime time.Duration) (Mapping, error) {
	return m.request(ctx, internalPort, externalPort, uint32(lifetime/time.Second))
}

func (m *pcpMapper) remove(ctx context.Context, mapping Mapping) error {
	_, err := m.request(ctx, mapping.InternalPort, 0, 0)
	return err
}

func (m *pcpMapper) request(ctx context.Context, internalPort, externalPort int, lifetimeSeconds uint32) (Mapping, error) {
	m.once.Do(func() { _, _ = rand.Read(m.nonce[:]) })
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(m.gateway))
	if err != nil {
		return Mapping{}, err
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return Mapping{}, errors.New("PCP client address is unavailable")
	}
	packet := make([]byte, pcpMapPacketSize)
	packet[0] = pcpVersion
	packet[1] = pcpOpcodeMap
	binary.BigEndian.PutUint32(packet[4:8], lifetimeSeconds)
	// As16 of an IPv4 address is its IPv4-mapped form, which PCP expects.
	clientIP := local.AddrPort().Addr().Unmap().As16()
	copy(packet[8:24], clientIP[:])
	copy(packet[24:36], m.nonce[:])
	packet[36] = pcpProtocolUDP
	binary.BigEndian.PutUint16(packet[40:42], uint16(internalPort))
	binary.BigEndian.PutUint16(packet[42:44], uint16(externalPort))
	// Suggested external address: all zeros as an IPv4-mapped address means
	// "any IPv4 address".
	packet[54], packet[55] = 0xff, 0xff

	response := make([]byte, 1100)
	timeout := pcpInitialTimeout
	for {
		if err := ctx.Err(); err != nil {
			return Mapping{}, err
		}
		deadline := time.Now().Add(timeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		_ = conn.SetDeadline(deadline)
		if _, err := conn.Write(packet); err != nil {
			return Mapping{}, err
		}
		count, err := conn.Read(response)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				timeout *= 2
				continue
			}
			return Mapping{}, err
		}
		mapping, retry, err := m.parse(response[:count], internalPort)
		if retry {
			continue
		}
		return mapping, err
	}
}

// parse decodes a MAP response. retry reports a datagram that does not
// answer this request (wrong opcode or nonce), which is ignored.
func (m *pcpMapper) parse(response []byte, internalPort int) (mapping Mapping, retry bool, err error) {
	if len(response) >= 4 && response[0] != pcpVersion {
		// A NAT-PMP-only server answers a version-2 request with its own
		// version 0 and UNSUPP_VERSION.
		return Mapping{}, false, fmt.Errorf("router does not speak PCP (version %d reply)", response[0])
	}
	if len(response) < pcpMapPacketSize || response[1] != pcpResponseBit|pcpOpcodeMap {
		return Mapping{}, true, nil
	}
	if result := response[3]; result != pcpResultSuccess {
		name := pcpResultNames[result]
		if name == "" {
			name = fmt.Sprintf("result %d", result)
		}
		return Mapping{}, false, fmt.Errorf("PCP MAP refused: %s", name)
	}
	if [12]byte(response[24:36]) != m.nonce {
		return Mapping{}, true, nil
	}
	lifetime := time.Duration(binary.BigEndian.Uint32(response[4:8])) * time.Second
	externalPort := binary.BigEndian.Uint16(response[42:44])
	external := netip.AddrFrom16([16]byte(response[44:60])).Unmap()
	return Mapping{
		Protocol: ProtocolPCP, InternalPort: internalPort,
		External: netip.AddrPortFrom(external, externalPort), Lifetime: lifetime,
		Advertised: Advertisable(external),
	}, false, nil
}
