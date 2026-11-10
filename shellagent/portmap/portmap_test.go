package portmap

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type pcpRequest struct {
	internalPort, externalPort uint16
	lifetime                   uint32
	nonce                      [12]byte
	clientIP                   netip.Addr
}

// fakePCPServer answers MAP requests with a fixed external address.
type fakePCPServer struct {
	conn     *net.UDPConn
	external netip.Addr
	mu       sync.Mutex
	requests []pcpRequest
}

func startFakePCP(t *testing.T, external string) *fakePCPServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := &fakePCPServer{conn: conn, external: netip.MustParseAddr(external)}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 1100)
		for {
			count, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			packet := buffer[:count]
			if count != pcpMapPacketSize || packet[0] != pcpVersion || packet[1] != pcpOpcodeMap || packet[36] != pcpProtocolUDP {
				continue
			}
			request := pcpRequest{
				lifetime:     binary.BigEndian.Uint32(packet[4:8]),
				clientIP:     netip.AddrFrom16([16]byte(packet[8:24])).Unmap(),
				nonce:        [12]byte(packet[24:36]),
				internalPort: binary.BigEndian.Uint16(packet[40:42]),
				externalPort: binary.BigEndian.Uint16(packet[42:44]),
			}
			server.mu.Lock()
			server.requests = append(server.requests, request)
			server.mu.Unlock()
			response := make([]byte, pcpMapPacketSize)
			copy(response, packet)
			response[1] = pcpResponseBit | pcpOpcodeMap
			response[2], response[3] = 0, pcpResultSuccess
			binary.BigEndian.PutUint32(response[4:8], request.lifetime)
			binary.BigEndian.PutUint32(response[8:12], 1234)
			clear(response[12:24])
			port := request.externalPort
			if port == 0 {
				port = request.internalPort
			}
			binary.BigEndian.PutUint16(response[42:44], port)
			external := server.external.As16()
			copy(response[44:60], external[:])
			_, _ = conn.WriteToUDP(response, from)
		}
	}()
	return server
}

func (s *fakePCPServer) port() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

func (s *fakePCPServer) snapshot() []pcpRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pcpRequest(nil), s.requests...)
}

func TestPCPMapsRenewsAndDeletes(t *testing.T) {
	server := startFakePCP(t, "203.0.113.7")
	client := New(Options{
		Gateway: netip.MustParseAddr("127.0.0.1"), PCPPort: server.port(),
		Protocols: []string{ProtocolPCP}, Lifetime: 2 * time.Second, Timeout: time.Second,
	})
	changes := make(chan *Mapping, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx, 9012, func(mapping *Mapping) { changes <- mapping })
		close(done)
	}()
	var mapping *Mapping
	select {
	case mapping = <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("no PCP mapping")
	}
	if mapping == nil || mapping.Protocol != ProtocolPCP || mapping.External != netip.MustParseAddrPort("203.0.113.7:9012") || !mapping.Advertised || mapping.InternalPort != 9012 {
		t.Fatalf("unexpected mapping: %#v", mapping)
	}
	if status := client.Status(); status.State != "mapped" || status.Protocol != ProtocolPCP || status.External != "203.0.113.7:9012" || !status.Public {
		t.Fatalf("unexpected status: %#v", status)
	}
	// Lease 2 s → renewed after 1 s with the same nonce and external port.
	deadline := time.Now().Add(5 * time.Second)
	for len(server.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	requests := server.snapshot()
	if len(requests) < 2 {
		t.Fatalf("mapping was not renewed: %d requests", len(requests))
	}
	if requests[1].nonce != requests[0].nonce || requests[1].externalPort != 9012 || requests[1].lifetime != 2 {
		t.Fatalf("renewal differs from the original request: %#v vs %#v", requests[1], requests[0])
	}
	if requests[0].internalPort != 9012 || requests[0].externalPort != 9012 || requests[0].clientIP != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("unexpected first request: %#v", requests[0])
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	requests = server.snapshot()
	last := requests[len(requests)-1]
	if last.lifetime != 0 || last.nonce != requests[0].nonce || last.internalPort != 9012 {
		t.Fatalf("mapping was not deleted: %#v", last)
	}
	select {
	case removed := <-changes:
		if removed != nil {
			t.Fatalf("expected nil change on delete, got %#v", removed)
		}
	case <-time.After(time.Second):
		t.Fatal("no change reported on delete")
	}
	if status := client.Status(); status.State != "stopped" {
		t.Fatalf("unexpected status after stop: %#v", status)
	}
}

func TestPrivateExternalAddressIsReportedButNotAdvertised(t *testing.T) {
	for _, external := range []string{"192.168.1.50", "10.2.3.4", "172.20.0.1", "100.72.1.1"} {
		server := startFakePCP(t, external)
		client := New(Options{
			Gateway: netip.MustParseAddr("127.0.0.1"), PCPPort: server.port(),
			Protocols: []string{ProtocolPCP}, Timeout: time.Second,
		})
		changes := make(chan *Mapping, 4)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			client.Run(ctx, 9012, func(mapping *Mapping) { changes <- mapping })
			close(done)
		}()
		select {
		case mapping := <-changes:
			if mapping == nil || mapping.Advertised || mapping.External.Addr() != netip.MustParseAddr(external) {
				t.Fatalf("%s: unexpected mapping %#v", external, mapping)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no mapping", external)
		}
		if status := client.Status(); status.State != "mapped" || status.Public || !strings.HasPrefix(status.External, external+":") {
			t.Fatalf("%s: unexpected status %#v", external, status)
		}
		cancel()
		<-done
	}
}

func TestAdvertisable(t *testing.T) {
	cases := map[string]bool{
		"203.0.113.7": true, "8.8.8.8": true, "100.63.255.255": true, "100.128.0.1": true,
		"192.168.0.1": false, "10.0.0.1": false, "172.16.5.5": false, "100.64.0.1": false,
		"100.127.255.254": false, "127.0.0.1": false, "169.254.1.1": false, "0.0.0.0": false,
	}
	for text, want := range cases {
		if got := Advertisable(netip.MustParseAddr(text)); got != want {
			t.Errorf("Advertisable(%s) = %v, want %v", text, got, want)
		}
	}
}

type natpmpRequest struct {
	opcode                     byte
	internalPort, externalPort uint16
	lifetime                   uint32
}

// startFakeNATPMP listens on the NAT-PMP port on loopback (go-nat-pmp has no
// port override). It answers PCP requests the way a NAT-PMP-only router does:
// version 0, UNSUPP_VERSION.
func startFakeNATPMP(t *testing.T, external [4]byte) (*sync.Mutex, *[]natpmpRequest, *int) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5351})
	if err != nil {
		t.Skipf("cannot bind the NAT-PMP port on loopback: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var mu sync.Mutex
	var requests []natpmpRequest
	pcpRequests := 0
	go func() {
		buffer := make([]byte, 1100)
		for {
			count, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			packet := buffer[:count]
			if count >= 2 && packet[0] == pcpVersion {
				mu.Lock()
				pcpRequests++
				mu.Unlock()
				_, _ = conn.WriteToUDP([]byte{0, 0x80 | packet[1], 0, 1, 0, 0, 0, 0}, from)
				continue
			}
			if count < 2 || packet[0] != 0 {
				continue
			}
			switch packet[1] {
			case 0:
				response := make([]byte, 12)
				response[1] = 0x80
				copy(response[8:12], external[:])
				_, _ = conn.WriteToUDP(response, from)
			case 1:
				if count != 12 {
					continue
				}
				request := natpmpRequest{
					opcode:       packet[1],
					internalPort: binary.BigEndian.Uint16(packet[4:6]),
					externalPort: binary.BigEndian.Uint16(packet[6:8]),
					lifetime:     binary.BigEndian.Uint32(packet[8:12]),
				}
				mu.Lock()
				requests = append(requests, request)
				mu.Unlock()
				response := make([]byte, 16)
				response[1] = 0x81
				binary.BigEndian.PutUint16(response[8:10], request.internalPort)
				mapped := request.externalPort
				if request.lifetime == 0 {
					mapped = 0
				}
				binary.BigEndian.PutUint16(response[10:12], mapped)
				binary.BigEndian.PutUint32(response[12:16], request.lifetime)
				_, _ = conn.WriteToUDP(response, from)
			}
		}
	}()
	return &mu, &requests, &pcpRequests
}

func TestFallsBackFromPCPToNATPMPAndDeletes(t *testing.T) {
	mu, requests, pcpRequests := startFakeNATPMP(t, [4]byte{198, 51, 100, 23})
	client := New(Options{
		Gateway: netip.MustParseAddr("127.0.0.1"), Protocols: []string{ProtocolPCP, ProtocolNATPMP},
		Timeout: 2 * time.Second,
	})
	changes := make(chan *Mapping, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx, 9012, func(mapping *Mapping) { changes <- mapping })
		close(done)
	}()
	select {
	case mapping := <-changes:
		if mapping == nil || mapping.Protocol != ProtocolNATPMP || mapping.External != netip.MustParseAddrPort("198.51.100.23:9012") || !mapping.Advertised || mapping.Lifetime != DefaultLifetime {
			t.Fatalf("unexpected mapping: %#v", mapping)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no NAT-PMP mapping")
	}
	if status := client.Status(); status.Protocol != ProtocolNATPMP || len(status.Tried) != 2 || status.Tried[0] != ProtocolPCP {
		t.Fatalf("unexpected status: %#v", status)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if *pcpRequests == 0 {
		t.Fatal("PCP was not tried first")
	}
	if len(*requests) != 2 {
		t.Fatalf("expected map + delete, got %#v", *requests)
	}
	if first := (*requests)[0]; first.internalPort != 9012 || first.externalPort != 9012 || first.lifetime != uint32(DefaultLifetime/time.Second) {
		t.Fatalf("unexpected map request: %#v", first)
	}
	if last := (*requests)[1]; last.internalPort != 9012 || last.lifetime != 0 || last.externalPort != 0 {
		t.Fatalf("unexpected delete request: %#v", last)
	}
}

// fakeIGD serves an IGDv1 root description with a WANIPConnection:1 service
// and records SOAP actions.
type fakeIGD struct {
	mu       sync.Mutex
	actions  []string
	bodies   []string
	external string
}

const fakeIGDRoot = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
<friendlyName>Fake router</friendlyName><UDN>uuid:fake-igd</UDN>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType><UDN>uuid:fake-wan</UDN>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType><UDN>uuid:fake-wanconn</UDN>
<serviceList><service>
<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
<serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
<controlURL>/ctl/IPConn</controlURL><eventSubURL>/evt/IPConn</eventSubURL><SCPDURL>/WANIPCn.xml</SCPDURL>
</service></serviceList>
</device></deviceList>
</device></deviceList>
</device>
</root>`

func (igd *fakeIGD) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/rootDesc.xml":
		response.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(response, fakeIGDRoot)
	case "/ctl/IPConn":
		action := request.Header.Get("SOAPAction")
		action = strings.Trim(action, `"`)
		name := action[strings.LastIndex(action, "#")+1:]
		body, _ := io.ReadAll(request.Body)
		igd.mu.Lock()
		igd.actions = append(igd.actions, name)
		igd.bodies = append(igd.bodies, string(body))
		igd.mu.Unlock()
		inner := ""
		if name == "GetExternalIPAddress" {
			inner = "<NewExternalIPAddress>" + igd.external + "</NewExternalIPAddress>"
		}
		response.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = io.WriteString(response, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:`+name+`Response xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`+inner+`</u:`+name+`Response></s:Body></s:Envelope>`)
	default:
		http.NotFound(response, request)
	}
}

func soapField(body, field string) string {
	decoder := xml.NewDecoder(strings.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == field {
			var value string
			_ = decoder.DecodeElement(&value, &start)
			return value
		}
	}
}

func TestUPnPMapsThroughIGDAndDeletes(t *testing.T) {
	igd := &fakeIGD{external: "203.0.113.44"}
	server := httptest.NewServer(igd)
	t.Cleanup(server.Close)
	client := New(Options{UPnPRootURL: server.URL + "/rootDesc.xml", Protocols: []string{ProtocolUPnP}, Timeout: 2 * time.Second})
	changes := make(chan *Mapping, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx, 9012, func(mapping *Mapping) { changes <- mapping })
		close(done)
	}()
	select {
	case mapping := <-changes:
		if mapping == nil || mapping.Protocol != ProtocolUPnP || mapping.External != netip.MustParseAddrPort("203.0.113.44:9012") || !mapping.Advertised {
			t.Fatalf("unexpected mapping: %#v", mapping)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no UPnP mapping")
	}
	cancel()
	<-done
	igd.mu.Lock()
	defer igd.mu.Unlock()
	if len(igd.actions) != 3 || igd.actions[0] != "AddPortMapping" || igd.actions[1] != "GetExternalIPAddress" || igd.actions[2] != "DeletePortMapping" {
		t.Fatalf("unexpected SOAP actions: %v", igd.actions)
	}
	add := igd.bodies[0]
	if soapField(add, "NewExternalPort") != "9012" || soapField(add, "NewInternalPort") != "9012" || soapField(add, "NewProtocol") != "UDP" ||
		soapField(add, "NewInternalClient") != "127.0.0.1" || soapField(add, "NewLeaseDuration") != "7200" {
		t.Fatalf("unexpected AddPortMapping body: %s", add)
	}
	if remove := igd.bodies[2]; soapField(remove, "NewExternalPort") != "9012" || soapField(remove, "NewProtocol") != "UDP" {
		t.Fatalf("unexpected DeletePortMapping body: %s", remove)
	}
}

func TestUnavailableRouterRetriesLater(t *testing.T) {
	// A closed loopback port: PCP fails and the client reports it.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	client := New(Options{
		Gateway: netip.MustParseAddr("127.0.0.1"), PCPPort: port, Protocols: []string{ProtocolPCP},
		Timeout: 500 * time.Millisecond, RetryInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx, 9012, func(mapping *Mapping) { t.Errorf("unexpected change %#v", mapping) })
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for client.Status().State != "unavailable" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if status := client.Status(); status.State != "unavailable" || status.Error == "" {
		t.Fatalf("unexpected status: %#v", status)
	}
	cancel()
	<-done
}
