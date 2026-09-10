package rdp

import "net"

func OpenBridge(host string, port int, token string) (net.Conn, error) {
	return openRDPBridge(host, port, token)
}

func RandomBridgeToken() string { return randomBridgeToken() }
