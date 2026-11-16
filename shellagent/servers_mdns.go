package shellagent

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// A minimal one-shot mDNS browser for `_dynapp._tcp` (RFC 6762/6763). It
// sends "QU" queries from an ephemeral port on every IPv4 interface, so
// responders answer by unicast and no port 5353 listener is needed. It only
// produces candidates: every address is verified by challenge before use.

const dynappMDNSService = "_dynapp._tcp.local."

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

type mdnsInstance struct {
	host   string
	port   int
	txt    map[string]string
	hasSRV bool
	hasTXT bool
}

type mdnsBrowser struct {
	mu        sync.Mutex
	instances map[string]*mdnsInstance
	addresses map[string][]net.IP
}

func (b *mdnsBrowser) instance(name string) *mdnsInstance {
	if b.instances[name] == nil {
		b.instances[name] = &mdnsInstance{txt: map[string]string{}}
	}
	return b.instances[name]
}

func (b *mdnsBrowser) absorb(packet []byte) {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil || !header.Response {
		return
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		resource, err := parser.Answer()
		if err != nil {
			if err != dnsmessage.ErrSectionDone {
				return
			}
			break
		}
		b.record(resource)
	}
	_ = parser.SkipAllAuthorities()
	for {
		resource, err := parser.Additional()
		if err != nil {
			return
		}
		b.record(resource)
	}
}

func (b *mdnsBrowser) record(resource dnsmessage.Resource) {
	name := strings.ToLower(resource.Header.Name.String())
	switch body := resource.Body.(type) {
	case *dnsmessage.PTRResource:
		if name == dynappMDNSService {
			b.instance(strings.ToLower(body.PTR.String()))
		}
	case *dnsmessage.SRVResource:
		if strings.HasSuffix(name, "."+dynappMDNSService) {
			instance := b.instance(name)
			instance.host = strings.ToLower(body.Target.String())
			instance.port = int(body.Port)
			instance.hasSRV = true
		}
	case *dnsmessage.TXTResource:
		if strings.HasSuffix(name, "."+dynappMDNSService) {
			instance := b.instance(name)
			instance.hasTXT = true
			for _, item := range body.TXT {
				key, value, _ := strings.Cut(item, "=")
				instance.txt[strings.ToLower(key)] = value
			}
		}
	case *dnsmessage.AResource:
		ip := net.IP(body.A[:]).To4()
		for _, known := range b.addresses[name] {
			if known.Equal(ip) {
				return
			}
		}
		b.addresses[name] = append(b.addresses[name], ip)
	}
}

// pendingQuestions asks for whatever the answers so far leave out.
func (b *mdnsBrowser) pendingQuestions() []dnsmessage.Question {
	b.mu.Lock()
	defer b.mu.Unlock()
	questions := []dnsmessage.Question{mdnsQuestion(dynappMDNSService, dnsmessage.TypePTR)}
	for name, instance := range b.instances {
		if !instance.hasSRV {
			questions = append(questions, mdnsQuestion(name, dnsmessage.TypeSRV))
		}
		if !instance.hasTXT {
			questions = append(questions, mdnsQuestion(name, dnsmessage.TypeTXT))
		}
		if instance.hasSRV && instance.host != "" && len(b.addresses[instance.host]) == 0 {
			questions = append(questions, mdnsQuestion(instance.host, dnsmessage.TypeA))
		}
	}
	if len(questions) > 16 {
		questions = questions[:16]
	}
	return questions
}

func mdnsQuestion(name string, kind dnsmessage.Type) dnsmessage.Question {
	parsed, _ := dnsmessage.NewName(name)
	// The top bit of the class asks for a unicast response (QU).
	return dnsmessage.Question{Name: parsed, Type: kind, Class: dnsmessage.ClassINET | 0x8000}
}

func (b *mdnsBrowser) candidates() []serverCandidate {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []serverCandidate
	for _, instance := range b.instances {
		if !instance.hasSRV || instance.port < 1 {
			continue
		}
		if version := instance.txt["v"]; version != "" && version != "1" {
			continue
		}
		scheme := strings.ToLower(instance.txt["scheme"])
		if scheme != "http" && scheme != "https" {
			scheme = ""
		}
		id := instance.txt["id"]
		if len(id) != 43 {
			id = ""
		}
		for _, ip := range b.addresses[instance.host] {
			result = append(result, serverCandidate{address: net.JoinHostPort(ip.String(), strconv.Itoa(instance.port)), scheme: scheme, instanceID: id, source: "mdns"})
		}
	}
	return result
}

// browseDynappMDNS browses for `_dynapp._tcp` until ctx is done or 1.5 s
// have passed, whichever is first.
func browseDynappMDNS(ctx context.Context) []serverCandidate {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	browser := &mdnsBrowser{instances: map[string]*mdnsInstance{}, addresses: map[string][]net.IP{}}
	var connections []*net.UDPConn
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, ok := address.(*net.IPNet)
			if !ok || prefix.IP.To4() == nil || prefix.IP.IsLinkLocalUnicast() {
				continue
			}
			connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: prefix.IP.To4()})
			if err != nil {
				continue
			}
			packet := ipv4.NewPacketConn(connection)
			iface := iface
			_ = packet.SetMulticastInterface(&iface)
			_ = packet.SetMulticastTTL(255)
			connections = append(connections, connection)
			break
		}
	}
	if len(connections) == 0 {
		return nil
	}
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	var readers sync.WaitGroup
	for _, connection := range connections {
		readers.Add(1)
		go func(connection *net.UDPConn) {
			defer readers.Done()
			buffer := make([]byte, 9000)
			for ctx.Err() == nil {
				_ = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _, err := connection.ReadFromUDP(buffer)
				if err != nil {
					continue
				}
				browser.absorb(buffer[:n])
			}
		}(connection)
	}
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		query := dnsmessage.Message{Questions: browser.pendingQuestions()}
		if packet, err := query.Pack(); err == nil {
			for _, connection := range connections {
				_, _ = connection.WriteToUDP(packet, mdnsGroup)
			}
		}
		select {
		case <-ctx.Done():
			readers.Wait()
			return browser.candidates()
		case <-ticker.C:
		}
	}
}
