package rdp

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"syscall"
	"time"
)

type rdcCleanPathRequest struct {
	Destination, ProxyAuth string
	X224                   []byte
}

func derLength(data []byte, offset int) (int, int, error) {
	if offset >= len(data) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	first := int(data[offset])
	if first < 128 {
		return first, 1, nil
	}
	count := first & 0x7f
	if count < 1 || count > 4 || offset+1+count > len(data) {
		return 0, 0, errors.New("invalid DER length")
	}
	value := 0
	for _, b := range data[offset+1 : offset+1+count] {
		value = value<<8 | int(b)
	}
	return value, 1 + count, nil
}
func derTLV(data []byte, offset int) (byte, []byte, int, error) {
	if offset >= len(data) {
		return 0, nil, offset, io.ErrUnexpectedEOF
	}
	length, lengthBytes, err := derLength(data, offset+1)
	if err != nil {
		return 0, nil, offset, err
	}
	start := offset + 1 + lengthBytes
	end := start + length
	if end > len(data) {
		return 0, nil, offset, io.ErrUnexpectedEOF
	}
	return data[offset], data[start:end], end, nil
}
func derExplicit(root []byte, tag byte) ([]byte, bool) {
	for offset := 0; offset < len(root); {
		current, value, next, err := derTLV(root, offset)
		if err != nil {
			return nil, false
		}
		if current == tag {
			return value, true
		}
		offset = next
	}
	return nil, false
}
func derInner(value []byte, expected byte) ([]byte, error) {
	tag, inner, next, err := derTLV(value, 0)
	if err != nil || tag != expected || next != len(value) {
		return nil, errors.New("invalid RDCleanPath field")
	}
	return inner, nil
}

func parseRdcCleanPathRequest(data []byte) (rdcCleanPathRequest, error) {
	tag, root, _, err := derTLV(data, 0)
	if err != nil || tag != 0x30 {
		return rdcCleanPathRequest{}, errors.New("invalid RDCleanPath request")
	}
	versionField, ok := derExplicit(root, 0xa0)
	if !ok {
		return rdcCleanPathRequest{}, errors.New("incomplete RDCleanPath request")
	}
	version, err := derInner(versionField, 0x02)
	if err != nil || len(version) < 1 {
		return rdcCleanPathRequest{}, errors.New("incomplete RDCleanPath request")
	}
	number := 0
	for _, b := range version {
		number = number<<8 | int(b)
	}
	if number != 3390 {
		return rdcCleanPathRequest{}, errors.New("incomplete RDCleanPath request")
	}
	destinationField, ok := derExplicit(root, 0xa2)
	if !ok {
		return rdcCleanPathRequest{}, errors.New("incomplete RDCleanPath request")
	}
	destination, err := derInner(destinationField, 0x0c)
	if err != nil {
		return rdcCleanPathRequest{}, err
	}
	x224Field, ok := derExplicit(root, 0xa6)
	if !ok {
		return rdcCleanPathRequest{}, errors.New("incomplete RDCleanPath request")
	}
	x224, err := derInner(x224Field, 0x04)
	if err != nil {
		return rdcCleanPathRequest{}, err
	}
	proxy := ""
	if field, present := derExplicit(root, 0xa3); present {
		if value, parseErr := derInner(field, 0x0c); parseErr == nil {
			proxy = string(value)
		}
	}
	return rdcCleanPathRequest{Destination: string(destination), ProxyAuth: proxy, X224: append([]byte(nil), x224...)}, nil
}
func derEncodeLength(length int) []byte {
	if length < 128 {
		return []byte{byte(length)}
	}
	if length <= 255 {
		return []byte{0x81, byte(length)}
	}
	return []byte{0x82, byte(length >> 8), byte(length)}
}
func derEncode(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derEncodeLength(len(content))...)
	return append(out, content...)
}
func derExplicitValue(tag byte, inner []byte) []byte { return derEncode(0xa0+tag, inner) }
func encodeRdcCleanPathResponse(address string, x224 []byte, certificates [][]byte) []byte {
	root := derExplicitValue(0, derEncode(0x02, []byte{0x0d, 0x3e}))
	root = append(root, derExplicitValue(6, derEncode(0x04, x224))...)
	chain := []byte{}
	for _, cert := range certificates {
		chain = append(chain, derEncode(0x04, cert)...)
	}
	root = append(root, derExplicitValue(7, derEncode(0x30, chain))...)
	root = append(root, derExplicitValue(9, derEncode(0x0c, []byte(address)))...)
	return derEncode(0x30, root)
}

// encodeRdcCleanPathError keeps proxy-side connection failures inside the
// RDCleanPath protocol. Closing the bridge without a response makes IronRDP
// report the misleading "not enough bytes" framing error instead.
func encodeRdcCleanPathError(wsaError uint16) []byte {
	root := derExplicitValue(0, derEncode(0x02, []byte{0x0d, 0x3e}))
	errorFields := derExplicitValue(0, derEncode(0x02, []byte{0x01}))
	if wsaError != 0 {
		value := []byte{byte(wsaError >> 8), byte(wsaError)}
		if value[0]&0x80 != 0 {
			value = append([]byte{0}, value...)
		}
		errorFields = append(errorFields, derExplicitValue(2, derEncode(0x02, value))...)
	}
	root = append(root, derExplicitValue(1, derEncode(0x30, errorFields))...)
	return derEncode(0x30, root)
}

func rdpWSAError(err error) uint16 {
	switch {
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return 10013 // permission denied (including macOS Local Network privacy)
	case errors.Is(err, syscall.ENETUNREACH):
		return 10051
	case errors.Is(err, syscall.ECONNRESET):
		return 10054
	case errors.Is(err, syscall.ETIMEDOUT):
		return 10060
	case errors.Is(err, syscall.ECONNREFUSED):
		return 10061
	case errors.Is(err, syscall.EHOSTDOWN):
		return 10064
	case errors.Is(err, syscall.EHOSTUNREACH):
		return 10065
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return 11001
	}
	return 0
}

func writeRdcCleanPathError(client net.Conn, err error) {
	_, _ = client.Write(encodeRdcCleanPathError(rdpWSAError(err)))
}

func readDER(reader io.Reader, limit int) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	lengthBytes := []byte{header[1]}
	if header[1]&0x80 != 0 {
		count := int(header[1] & 0x7f)
		if count < 1 || count > 4 {
			return nil, errors.New("invalid DER length")
		}
		extra := make([]byte, count)
		if _, err := io.ReadFull(reader, extra); err != nil {
			return nil, err
		}
		lengthBytes = append(lengthBytes, extra...)
	}
	length, consumed, err := derLength(lengthBytes, 0)
	if err != nil {
		return nil, err
	}
	prefix := append([]byte{header[0]}, lengthBytes[:consumed]...)
	if length > limit {
		return nil, errors.New("RDCleanPath request is too large")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return append(prefix, body...), nil
}
func readTPKT(reader io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(header[2:]))
	if length < 4 || length > 65535 {
		return nil, errors.New("invalid X.224 response")
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return append(header, body...), nil
}

func randomBridgeToken() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}
func openRDPBridge(host string, port int, token string) (net.Conn, error) {
	if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " /\\?#\x00") || port < 1 || port > 65535 {
		return nil, errors.New("RDP target is invalid")
	}
	agent, handler := net.Pipe()
	go runRDPBridge(handler, host, port, token)
	return agent, nil
}

func rdpTLSConfig(host string) *tls.Config {
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
	}
}

func legacyRDPTLSConfig(host string) *tls.Config {
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
	}
}

func isRDPProtocolVersionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "protocol version not supported") ||
		strings.Contains(message, "server selected unsupported protocol version")
}

func preferTLSOnlyRDPNegotiation(value []byte) []byte {
	request := append([]byte(nil), value...)
	for offset := 11; offset+8 <= len(request); offset++ {
		if request[offset] != 0x01 || binary.LittleEndian.Uint16(request[offset+2:]) != 8 {
			continue
		}
		protocols := binary.LittleEndian.Uint32(request[offset+4:])
		if protocols&0x01 == 0 {
			return request
		}
		// Some old Windows 7 hosts select HYBRID and then abort when modern
		// CredSSP begins. Keep the TLS transport while removing HYBRID and
		// HYBRID_EX from this compatibility retry.
		binary.LittleEndian.PutUint32(request[offset+4:], 0x01)
		return request
	}
	return request
}

func dialRDPX224(address string, request []byte) (net.Conn, []byte, error) {
	tcp, err := net.DialTimeout("tcp", address, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if _, err = tcp.Write(request); err != nil {
		tcp.Close()
		return nil, nil, err
	}
	x224, err := readTPKT(tcp)
	if err != nil {
		tcp.Close()
		return nil, nil, err
	}
	return tcp, x224, nil
}

func runRDPBridge(client net.Conn, host string, port int, token string) {
	defer client.Close()
	requestBytes, err := readDER(client, 32*1024*1024)
	if err != nil {
		return
	}
	request, err := parseRdcCleanPathRequest(requestBytes)
	if err != nil || request.ProxyAuth != token || (request.Destination != host && request.Destination != net.JoinHostPort(host, fmt.Sprint(port))) {
		return
	}
	address := net.JoinHostPort(host, fmt.Sprint(port))
	tcp, x224, err := dialRDPX224(address, request.X224)
	if err != nil {
		writeRdcCleanPathError(client, err)
		return
	}
	// RDP's TLS transport is consumed by CredSSP/NLA. Some Windows RDP servers
	// advertise TLS 1.3 but abort immediately after the handshake when it is
	// selected, before the first CredSSP frame. Native RDP clients cap this
	// transport at TLS 1.2, so do the same for RDCleanPath.
	secure := tls.Client(tcp, rdpTLSConfig(host))
	if err = secure.Handshake(); err != nil {
		tcp.Close()
		if !isRDPProtocolVersionError(err) {
			log.Printf("RDP bridge %s: modern TLS handshake failed: %v", address, err)
			writeRdcCleanPathError(client, err)
			return
		}
		// A few older Windows hosts only accept TLS 1.0 with RSA/AES-CBC.
		// Reconnect because the rejected TLS handshake consumed the first TCP
		// transport, and keep this downgrade behind the explicit version alert.
		log.Printf("RDP bridge %s: retrying legacy TLS after protocol-version rejection", address)
		tcp, x224, err = dialRDPX224(address, preferTLSOnlyRDPNegotiation(request.X224))
		if err != nil {
			writeRdcCleanPathError(client, err)
			return
		}
		secure = tls.Client(tcp, legacyRDPTLSConfig(host))
		if err = secure.Handshake(); err != nil {
			tcp.Close()
			writeRdcCleanPathError(client, err)
			return
		}
	}
	state := secure.ConnectionState()
	log.Printf("RDP bridge %s: TLS %x cipher %x", address, state.Version, state.CipherSuite)
	certificates := make([][]byte, 0, len(state.PeerCertificates))
	for _, certificate := range state.PeerCertificates {
		certificates = append(certificates, certificate.Raw)
	}
	if _, err = client.Write(encodeRdcCleanPathResponse(address, x224, certificates)); err != nil {
		secure.Close()
		return
	}
	type copyResult struct {
		direction string
		bytes     int64
		err       error
	}
	done := make(chan copyResult, 2)
	go func() {
		n, copyErr := io.Copy(secure, client)
		closeErr := secure.CloseWrite()
		if copyErr == nil {
			copyErr = closeErr
		}
		done <- copyResult{"browser-to-rdp", n, copyErr}
	}()
	go func() {
		n, copyErr := io.Copy(client, secure)
		done <- copyResult{"rdp-to-browser", n, copyErr}
	}()
	result := <-done
	log.Printf("RDP bridge %s: %s ended after %d bytes: %v", address, result.direction, result.bytes, result.err)
	_ = secure.Close()
	result = <-done
	log.Printf("RDP bridge %s: %s ended after %d bytes: %v", address, result.direction, result.bytes, result.err)
}
