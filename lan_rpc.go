package shellagent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func (s *Server) handleLANRPC(request message) (any, error) {
	switch request.Method {
	case "status":
		return s.lanStatus()
	case "setEnabled":
		enabled := false
		if len(request.Args) > 0 {
			enabled, _ = request.Args[0].(bool)
		}
		mode := ListenerLocal
		name := ""
		if len(request.Args) > 1 {
			if options, ok := request.Args[1].(map[string]any); ok {
				if value, _ := options["listenerMode"].(string); value != "" {
					mode = value
				}
				if value, _ := options["name"].(string); value != "" {
					name = value
				}
			}
		}
		if !enabled {
			mode = ListenerOff
		} else if mode == ListenerOff {
			mode = ListenerLocal
		}
		return s.setListener(mode, name)
	case "createPairing", "revokePairing":
		return nil, errors.New("LAN browser trust is managed through Dyner pairing")
	default:
		return nil, errors.New("unsupported LAN method")
	}
}

func (s *Server) lanStatus() (map[string]any, error) {
	s.mu.Lock()
	running := s.lanClose != nil
	config := s.Config
	s.mu.Unlock()
	hash, err := s.LANCertificateHash()
	if err != nil {
		return nil, err
	}
	mode := config.resolvedListenerMode()
	address := config.LANAddress
	if address == "" {
		if mode == ListenerLAN {
			address = "0.0.0.0:9011"
		} else {
			address = DefaultAddress
		}
	}
	port := portFromAddress(address)
	var endpoints []string
	var endpoint any
	if mode == ListenerLAN {
		endpoints = lanEndpoints(address, port, config.DynerBaseURL)
		if len(endpoints) > 0 {
			endpoint = endpoints[0]
		}
	}
	direct := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + RemotePath
	return map[string]any{
		"supported": true, "enabled": mode != ListenerOff, "running": running,
		"listenerMode": mode, "host": hostFromAddress(address), "port": port,
		"protocolVersion": ProtocolVersion, "directEndpoint": direct,
		"lanEndpoint": endpoint, "lanEndpoints": endpoints, "lanCarrier": "webtransport",
		"proxyPath": RemotePath, "serverCertificateHashes": []any{hash}, "pairingLinks": []any{},
	}, nil
}

func hostFromAddress(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

func preferredLanHost(dynerBase string) string {
	parsed, err := url.Parse(strings.TrimSpace(dynerBase))
	if err != nil {
		return ""
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.String()
}

func lanHostRank(host, preferred string) int {
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return 100
	}
	if preferred != "" && host == preferred {
		return 0
	}
	v4 := ip.To4()
	if v4 == nil {
		return 90
	}
	if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return 70
	}
	if ip.IsPrivate() {
		if v4[0] == 192 && v4[1] == 168 {
			return 10
		}
		if v4[0] == 10 {
			return 20
		}
		return 30
	}
	return 50
}

func sortLanHosts(hosts []string, preferred string) []string {
	seen := map[string]bool{}
	ranked := []string{}
	for _, candidate := range hosts {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		ranked = append(ranked, candidate)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := lanHostRank(ranked[i], preferred), lanHostRank(ranked[j], preferred)
		if left != right {
			return left < right
		}
		return ranked[i] < ranked[j]
	})
	return ranked
}

func lanEndpoints(address string, port int, dynerBase string) []string {
	host := hostFromAddress(address)
	hosts := []string{host}
	if host == "0.0.0.0" || host == "::" || host == "" {
		hosts = nil
		interfaces, _ := net.Interfaces()
		for _, networkInterface := range interfaces {
			if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addresses, _ := networkInterface.Addrs()
			for _, address := range addresses {
				ip, _, _ := net.ParseCIDR(address.String())
				if ip != nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
					hosts = append(hosts, ip.String())
				}
			}
		}
	}
	endpoints := []string{}
	for _, candidate := range sortLanHosts(hosts, preferredLanHost(dynerBase)) {
		endpoints = append(endpoints, "https://"+net.JoinHostPort(candidate, strconv.Itoa(port))+RemotePath)
	}
	return endpoints
}

func portFromAddress(address string) int {
	parsed, err := url.Parse("udp://" + address)
	if err != nil {
		return 9011
	}
	port := parsed.Port()
	number, _ := strconv.Atoi(port)
	if number == 0 {
		return 9011
	}
	return number
}

func (s *Server) setLANEnabled(enabled bool) (map[string]any, error) {
	mode := ListenerLocal
	if !enabled {
		mode = ListenerOff
	} else if s.Config.ListenerMode == ListenerLAN {
		mode = ListenerLAN
	}
	return s.setListener(mode, s.Config.ListenerName)
}

func (s *Server) setListener(mode, name string) (map[string]any, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = ListenerLocal
	}
	s.mu.Lock()
	loopback := s.Address
	if loopback == "" {
		loopback = DefaultAddress
	}
	if strings.TrimSpace(name) != "" {
		s.Config.ListenerName = strings.TrimSpace(name)
	}
	s.Config.ListenerMode = mode
	s.Config.ApplyListenerDefaults(loopback, mode == ListenerLAN)
	snapshot := s.Config
	s.mu.Unlock()
	if err := SaveConfig(s.StateDir, snapshot); err != nil {
		return nil, err
	}
	if err := s.restartLAN(); err != nil {
		return nil, err
	}
	if mode == ListenerOff {
		s.mu.Lock()
		token := s.AccountToken
		config := s.Config
		s.mu.Unlock()
		if token != "" && config.EnvironmentID != "" {
			_ = RevokeRemoteEnvironment(context.Background(), nil, config.DynerBaseURL, token, config.EnvironmentID)
		}
		s.mu.Lock()
		s.Config.EnvironmentID = ""
		s.Config.DeviceCredential = ""
		snapshot = s.Config
		s.mu.Unlock()
		_ = SaveConfig(s.StateDir, snapshot)
	} else if err := s.ensureListenerRegistration(context.Background()); err != nil {
		return nil, err
	}
	s.startIdentitySync()
	return s.lanStatus()
}
