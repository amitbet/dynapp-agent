package shellagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/portmap"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// meshICEPort is the fixed local UDP port answering peer connections share
// while a router port mapping is active (docs/mesh-network.md, section 2).
// 9011 is the LAN WebTransport listener.
const meshICEPort = 9012

// portMapRebindDelay limits how often a failed bind of meshICEPort (another
// process owns it) is retried.
const portMapRebindDelay = 10 * time.Minute

// meshTiming holds the inbox and sync intervals. Tests shorten them; nil
// fields use the defaults from the contract.
type meshTiming struct {
	sync        time.Duration // browser-identities sync while the inbox is down
	relaxedSync time.Duration // while the inbox is connected
	ping        time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration
}

func (s *Server) timing() meshTiming {
	timing := meshTiming{sync: remoteStateSyncInterval, relaxedSync: remoteStateRelaxedSyncInterval, ping: inboxPingInterval, backoffMin: time.Second, backoffMax: 60 * time.Second}
	if s.meshTimingOverride != nil {
		override := *s.meshTimingOverride
		if override.sync > 0 {
			timing.sync = override.sync
		}
		if override.relaxedSync > 0 {
			timing.relaxedSync = override.relaxedSync
		}
		if override.ping > 0 {
			timing.ping = override.ping
		}
		if override.backoffMin > 0 {
			timing.backoffMin = override.backoffMin
		}
		if override.backoffMax > 0 {
			timing.backoffMax = override.backoffMax
		}
	}
	return timing
}

// remoteSyncInterval is the browser-identities poll interval: relaxed while
// the inbox delivers events, the fast fallback otherwise.
func (s *Server) remoteSyncInterval() time.Duration {
	timing := s.timing()
	if s.inboxConnected.Load() {
		return timing.relaxedSync
	}
	return timing.sync
}

// requestRemoteSync runs the browser-identities sync loop once now.
func (s *Server) requestRemoteSync() {
	select {
	case s.syncWake() <- struct{}{}:
	default:
	}
}

func (s *Server) syncWake() chan struct{} {
	s.meshMu.Lock()
	defer s.meshMu.Unlock()
	if s.remoteSyncWake == nil {
		s.remoteSyncWake = make(chan struct{}, 1)
	}
	return s.remoteSyncWake
}

// meshWanted reports whether the inbox and port mapping should run: enrolled
// with remote access (relay) enabled, inside a running agent.
func (s *Server) meshConfig() (Config, bool) {
	s.mu.Lock()
	config := s.Config.relayTicketConfig()
	active := s.meshActive
	s.mu.Unlock()
	return config, active && config.RelayEnabled && config.DeviceCredential != "" && config.EnvironmentID != "" && config.DynerBaseURL != ""
}

// reconcileMesh starts or stops the inbox and the port mapping to match the
// current configuration. It is cheap and runs after every sync.
func (s *Server) reconcileMesh() {
	s.reconcileInbox()
	s.reconcilePortMapping()
}

func (s *Server) stopMesh() {
	s.stopInbox()
	s.stopPortMapping()
}

// iceMuxTransport is the shared ICE socket used while a mapping is
// advertised. Each answering PeerConnection gets it through its
// SettingEngine; the mux routes packets to the right one by ICE ufrag.
type iceMuxTransport struct {
	mux       ice.UDPMux
	localPort int
	protocol  string
	external  netip.AddrPort
}

// newICEUDPMux binds the shared ICE port for IPv4 (0.0.0.0) and, when the
// host allows it, IPv6 ([::], v6-only) on the same port, and joins them
// with pion's MultiUDPMuxDefault, which routes each host candidate to the
// socket that owns its address. IPv6 host candidates are a direct path of
// their own (mobile networks especially), so they must survive the switch
// to the shared port; when IPv6 cannot be bound the mux stays IPv4-only and
// the mapping is still advertised. port 0 picks a free port (tests).
//
// The parts are plain UDPMuxDefaults. The contract names a UniversalUDPMux,
// but its only addition, STUN server-reflexive discovery through the shared
// socket, is unreachable from webrtc v4 (the SettingEngine sets
// ice.AgentConfig.UDPMux, never UDPMuxSrflx), and NewUniversalUDPMuxDefault
// starts its read loop before it assigns the embedded mux, a data race that
// can dereference nil if a datagram arrives during construction.
// NewMultiUDPMuxFromPort is not used either: it binds one socket per
// interface address and fails as a whole when any one bind fails.
func newICEUDPMux(port int) (ice.UDPMux, int, error) {
	conn4, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		return nil, 0, err
	}
	localPort := conn4.LocalAddr().(*net.UDPAddr).Port
	muxes := []ice.UDPMux{familyUDPMux{ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn4}), false}}
	// Go sets IPV6_V6ONLY for "udp6", so this does not collide with the
	// IPv4 socket on the same port.
	if conn6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: localPort}); err == nil {
		muxes = append(muxes, familyUDPMux{ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn6}), true})
	} else {
		log.Printf("DynApp Shell agent: ICE port %d is IPv4-only: %v", localPort, err)
	}
	return ice.NewMultiUDPMuxDefault(muxes...), localPort, nil
}

// familyUDPMux limits one socket's host addresses to its own family and
// hides loopback. A mux on an unspecified address lists every interface
// address (pion enumerates IPv4 and IPv6 for [::]), and pion only skips
// loopback for a bare *ice.UDPMuxDefault.
type familyUDPMux struct {
	ice.UDPMux
	ipv6 bool
}

func (m familyUDPMux) GetListenAddresses() []net.Addr {
	var addresses []net.Addr
	for _, address := range m.UDPMux.GetListenAddresses() {
		udp, ok := address.(*net.UDPAddr)
		if !ok || udp.IP.IsLoopback() || (udp.IP.To4() == nil) != m.ipv6 {
			continue
		}
		addresses = append(addresses, address)
	}
	return addresses
}

func (t *iceMuxTransport) helloPortMapping() map[string]any {
	if t == nil || !t.external.IsValid() {
		return nil
	}
	return map[string]any{"protocol": t.protocol, "external": t.external.String()}
}

// apply routes this PeerConnection's host candidates through the shared mux.
// pion's STUN server-reflexive gathering is unchanged: webrtc v4 never hands
// the mux to ICE as UDPMuxSrflx, so STUN still runs on its own ephemeral
// sockets. SetNAT1To1IPs cannot add the mapped address either: it refuses
// STUN servers alongside it and binds a new ephemeral port rather than the
// mux. The mapped address is therefore added to the answer (mappedCandidate)
// and inbound checks to it reach the mux through the router.
func (t *iceMuxTransport) apply(settings *webrtc.SettingEngine) {
	if t != nil {
		settings.SetICEUDPMux(t.mux)
	}
}

// mappedCandidate is the server-reflexive candidate for the router mapping,
// in the media section identified by mid.
func (t *iceMuxTransport) mappedCandidate(mid string) webrtc.ICECandidateInit {
	// Type preference 100 (srflx), local preference 65535, component 1: the
	// same priority pion gives its STUN srflx candidates.
	const priority = 100<<24 | 65535<<8 | 255
	line := fmt.Sprintf("candidate:portmap 1 udp %d %s %d typ srflx raddr 0.0.0.0 rport %d",
		priority, t.external.Addr().String(), t.external.Port(), t.localPort)
	index := uint16(0)
	return webrtc.ICECandidateInit{Candidate: line, SDPMid: &mid, SDPMLineIndex: &index}
}

// sdpFirstMid returns the first media section's mid ("0" for a browser's
// data-channel-only offer).
func sdpFirstMid(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		if mid, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "a=mid:"); ok {
			return mid
		}
	}
	return "0"
}

// sdpWithCandidate adds an a=candidate line to the first media section,
// after its existing candidates and before a=end-of-candidates. An identical
// address/port already present (STUN found the same mapping) is left alone.
func sdpWithCandidate(sdp, candidate string) string {
	attribute := "a=" + candidate
	fields := strings.Fields(candidate)
	lines := strings.Split(strings.TrimSuffix(sdp, "\r\n"), "\r\n")
	start, end := -1, len(lines)
	insert := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "m=") {
			if start >= 0 {
				end = index
				break
			}
			start = index
			continue
		}
		if start < 0 {
			continue
		}
		if strings.HasPrefix(line, "a=candidate:") {
			existing := strings.Fields(line)
			if len(existing) >= 6 && len(fields) >= 6 && existing[4] == fields[4] && existing[5] == fields[5] && strings.EqualFold(existing[2], fields[2]) {
				return sdp
			}
			insert = index + 1
		}
		if line == "a=end-of-candidates" && insert < 0 {
			insert = index
		}
	}
	if start < 0 {
		return sdp
	}
	if insert < 0 {
		insert = end
	}
	lines = append(lines[:insert], append([]string{attribute}, lines[insert:]...)...)
	return strings.Join(lines, "\r\n") + "\r\n"
}

func (s *Server) currentICETransport() *iceMuxTransport {
	return s.iceTransport.Load()
}

func (s *Server) reconcilePortMapping() {
	_, want := s.meshConfig()
	s.meshMu.Lock()
	running := s.portmapCancel != nil
	if !want || running || time.Since(s.portmapBindFailedAt) < portMapRebindDelay {
		s.meshMu.Unlock()
		if !want && running {
			s.stopPortMapping()
		}
		return
	}
	port := s.portmapLocalPort
	if port == 0 {
		port = meshICEPort
	} else if port < 0 {
		port = 0 // tests: any free port
	}
	mux, localPort, err := newICEUDPMux(port)
	if err != nil {
		s.portmapBindFailedAt = time.Now()
		s.meshMu.Unlock()
		log.Printf("DynApp Shell agent: cannot bind ICE port %d for router port mapping: %v", port, err)
		return
	}
	options := portmap.Options{}
	if s.portmapOptions != nil {
		options = *s.portmapOptions
	}
	client := portmap.New(options)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.portmapCancel = cancel
	s.portmapDone = done
	s.portmapClient = client
	s.meshMu.Unlock()
	go func() {
		defer close(done)
		defer func() { _ = mux.Close() }()
		client.Run(ctx, localPort, func(mapping *portmap.Mapping) {
			if mapping == nil || !mapping.Advertised {
				s.iceTransport.Store(nil)
				if mapping != nil {
					log.Printf("DynApp Shell agent: router mapped UDP %d via %s to private address %s; not advertised", localPort, mapping.Protocol, mapping.External)
				}
				return
			}
			log.Printf("DynApp Shell agent: router mapped UDP %d via %s to %s", localPort, mapping.Protocol, mapping.External)
			s.iceTransport.Store(&iceMuxTransport{mux: mux, localPort: localPort, protocol: mapping.Protocol, external: mapping.External})
		})
		s.iceTransport.Store(nil)
	}()
}

// stopPortMapping deletes the router mapping and releases the ICE port. It
// waits for the delete, which portmap bounds by its request timeout.
func (s *Server) stopPortMapping() {
	s.meshMu.Lock()
	cancel, done := s.portmapCancel, s.portmapDone
	s.portmapCancel, s.portmapDone, s.portmapClient = nil, nil, nil
	s.meshMu.Unlock()
	if cancel == nil {
		return
	}
	s.iceTransport.Store(nil)
	cancel()
	<-done
}

// portMappingStatus is the settings-page view of the router mapping; nil
// while mapping is not running (remote access off or not enrolled).
func (s *Server) portMappingStatus() any {
	s.meshMu.Lock()
	client := s.portmapClient
	s.meshMu.Unlock()
	if client == nil {
		return nil
	}
	return client.Status()
}

// marshalCandidate encodes a candidate for Dyner's shell-candidates list.
func marshalCandidate(candidate webrtc.ICECandidateInit) json.RawMessage {
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil
	}
	return encoded
}
