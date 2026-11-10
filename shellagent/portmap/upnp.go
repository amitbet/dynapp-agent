package portmap

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

// upnpClient is the part of the WANIPConnection1/2 and WANPPPConnection1
// SOAP clients the mapper uses. All three generated clients satisfy it.
type upnpClient interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
}

type upnpService struct {
	client upnpClient
	// internal is this machine's address as the gateway sees it.
	internal netip.Addr
}

type upnpMapper struct {
	rootURL     string
	description string
	timeout     time.Duration
	mu          sync.Mutex
	service     *upnpService
}

func (m *upnpMapper) name() string { return ProtocolUPnP }

func (m *upnpMapper) add(ctx context.Context, internalPort, externalPort int, lifetime time.Duration) (Mapping, error) {
	service, err := m.discover(ctx)
	if err != nil {
		return Mapping{}, err
	}
	seconds := uint32(lifetime / time.Second)
	err = service.client.AddPortMappingCtx(ctx, "", uint16(externalPort), "UDP", uint16(internalPort), service.internal.String(), true, m.description, seconds)
	if err != nil {
		// Some IGDv1 routers only accept permanent mappings (error 725,
		// OnlyPermanentLeasesSupported); the agent deletes it on shutdown.
		if retryErr := service.client.AddPortMappingCtx(ctx, "", uint16(externalPort), "UDP", uint16(internalPort), service.internal.String(), true, m.description, 0); retryErr != nil {
			m.forget()
			return Mapping{}, err
		}
		seconds = uint32(DefaultLifetime / time.Second)
	}
	text, err := service.client.GetExternalIPAddressCtx(ctx)
	if err != nil {
		return Mapping{}, err
	}
	external, err := netip.ParseAddr(text)
	if err != nil {
		return Mapping{}, errors.New("router reported an invalid external address")
	}
	external = external.Unmap()
	return Mapping{
		Protocol: ProtocolUPnP, InternalPort: internalPort,
		External:   netip.AddrPortFrom(external, uint16(externalPort)),
		Lifetime:   time.Duration(seconds) * time.Second,
		Advertised: Advertisable(external),
	}, nil
}

func (m *upnpMapper) remove(ctx context.Context, mapping Mapping) error {
	service, err := m.discover(ctx)
	if err != nil {
		return err
	}
	return service.client.DeletePortMappingCtx(ctx, "", mapping.External.Port(), "UDP")
}

func (m *upnpMapper) forget() {
	m.mu.Lock()
	m.service = nil
	m.mu.Unlock()
}

func (m *upnpMapper) discover(ctx context.Context) (*upnpService, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.service != nil {
		return m.service, nil
	}
	var roots []upnpRoot
	if m.rootURL != "" {
		location, err := url.Parse(m.rootURL)
		if err != nil {
			return nil, err
		}
		root, err := goupnp.DeviceByURLCtx(ctx, location)
		if err != nil {
			return nil, err
		}
		roots = append(roots, upnpRoot{root: root, location: location})
	} else {
		roots = discoverIGDs(ctx)
	}
	for _, item := range roots {
		clients := upnpClientsFromRoot(item.root, item.location)
		if len(clients) == 0 {
			continue
		}
		internal := item.local
		if !internal.IsValid() {
			if host, err := netip.ParseAddr(item.location.Hostname()); err == nil {
				internal = localAddressToward(host.Unmap())
			} else if ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", item.location.Hostname()); err == nil && len(ips) > 0 {
				internal = localAddressToward(ips[0].Unmap())
			}
		}
		if !internal.IsValid() {
			continue
		}
		m.service = &upnpService{client: clients[0], internal: internal}
		return m.service, nil
	}
	return nil, errors.New("no UPnP internet gateway found")
}

type upnpRoot struct {
	root     *goupnp.RootDevice
	location *url.URL
	local    netip.Addr
}

// discoverIGDs searches for IGDv1 and IGDv2 devices in parallel; each SSDP
// search waits its full window, so doing them in turn doubles the delay.
func discoverIGDs(ctx context.Context) []upnpRoot {
	targets := []string{internetgateway2.URN_WANIPConnection_2, internetgateway2.URN_WANIPConnection_1, internetgateway2.URN_WANPPPConnection_1}
	results := make([][]goupnp.MaybeRootDevice, len(targets))
	var wg sync.WaitGroup
	for index, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := goupnp.DiscoverDevicesCtx(ctx, target)
			if err == nil {
				results[index] = found
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	var roots []upnpRoot
	for _, found := range results {
		for _, maybe := range found {
			if maybe.Err != nil || maybe.Root == nil || maybe.Location == nil || seen[maybe.Location.String()] {
				continue
			}
			seen[maybe.Location.String()] = true
			local, _ := netip.AddrFromSlice(maybe.LocalAddr)
			roots = append(roots, upnpRoot{root: maybe.Root, location: maybe.Location, local: local.Unmap()})
		}
	}
	return roots
}

// upnpClientsFromRoot prefers WANIPConnection2, then WANIPConnection1, then
// WANPPPConnection1 (DSL routers).
func upnpClientsFromRoot(root *goupnp.RootDevice, location *url.URL) []upnpClient {
	var clients []upnpClient
	if found, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, location); err == nil {
		for _, client := range found {
			clients = append(clients, client)
		}
	}
	if found, err := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(root, location); err == nil {
		for _, client := range found {
			clients = append(clients, client)
		}
	}
	if found, err := internetgateway2.NewWANPPPConnection1ClientsFromRootDevice(root, location); err == nil {
		for _, client := range found {
			clients = append(clients, client)
		}
	}
	return clients
}
