// Package portmap asks the home router for a UDP port mapping so a WebRTC
// peer outside the network can reach this machine directly. It speaks PCP
// (RFC 6887), NAT-PMP (RFC 6886) and UPnP-IGD, in that order, and keeps one
// mapping alive: renewed at half its lease and deleted when the caller stops.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jackpal/gateway"
)

// Protocol names as they appear in status and in the WebRTC hello reply.
const (
	ProtocolPCP    = "pcp"
	ProtocolNATPMP = "natpmp"
	ProtocolUPnP   = "upnp"
)

// DefaultLifetime is the lease requested from the router.
const DefaultLifetime = 2 * time.Hour

// DefaultRetryInterval is how long the client waits before asking again after
// every protocol failed (no router support, router unreachable).
const DefaultRetryInterval = 10 * time.Minute

// Mapping is one granted UDP mapping.
type Mapping struct {
	Protocol     string
	InternalPort int
	External     netip.AddrPort
	Lifetime     time.Duration
	// Advertised is false when the router reported a private or carrier-grade
	// NAT external address (double NAT): the mapping exists but a peer on the
	// internet still cannot reach it, so it is reported and not advertised.
	Advertised bool
}

// Status is the client's current view, for the settings page.
type Status struct {
	State    string   `json:"state"`
	Protocol string   `json:"protocol,omitempty"`
	External string   `json:"external,omitempty"`
	Internal int      `json:"internalPort,omitempty"`
	Public   bool     `json:"advertised"`
	Error    string   `json:"error,omitempty"`
	Expires  string   `json:"expiresAt,omitempty"`
	Tried    []string `json:"tried,omitempty"`
}

// Options configures a Client. The zero value maps through the default
// gateway with the protocol ports and lease from the specs.
type Options struct {
	// Gateway overrides default-route discovery.
	Gateway netip.Addr
	// PCPPort overrides the PCP server port (5351); tests use a loopback port.
	PCPPort int
	// UPnPRootURL skips SSDP discovery and reads this IGD root description.
	UPnPRootURL string
	// Protocols limits and orders the protocols tried (default: all three).
	Protocols []string
	// Lifetime is the requested lease (default DefaultLifetime).
	Lifetime time.Duration
	// RetryInterval is the wait after a failed attempt (default DefaultRetryInterval).
	RetryInterval time.Duration
	// Timeout bounds one protocol attempt (default 3 s).
	Timeout time.Duration
	// Description labels the UPnP mapping on the router's admin page.
	Description string
}

// mapper is one protocol's implementation.
type mapper interface {
	name() string
	add(ctx context.Context, internalPort, externalPort int, lifetime time.Duration) (Mapping, error)
	remove(ctx context.Context, mapping Mapping) error
}

// Client keeps one mapping for a fixed internal UDP port.
type Client struct {
	options Options
	mu      sync.Mutex
	status  Status
	// newMappers is replaced by tests that want to observe calls.
	newMappers func(ctx context.Context) []mapper
}

// New returns a client; Run starts mapping.
func New(options Options) *Client {
	if options.Lifetime <= 0 {
		options.Lifetime = DefaultLifetime
	}
	if options.RetryInterval <= 0 {
		options.RetryInterval = DefaultRetryInterval
	}
	if options.Timeout <= 0 {
		options.Timeout = 3 * time.Second
	}
	if options.PCPPort == 0 {
		options.PCPPort = pcpServerPort
	}
	if len(options.Protocols) == 0 {
		options.Protocols = []string{ProtocolPCP, ProtocolNATPMP, ProtocolUPnP}
	}
	if options.Description == "" {
		options.Description = "DynApp agent"
	}
	client := &Client{options: options, status: Status{State: "starting"}}
	client.newMappers = client.defaultMappers
	return client
}

// Status reports the current mapping state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.status
	status.Tried = append([]string(nil), c.status.Tried...)
	return status
}

func (c *Client) setStatus(status Status) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

// Run maps internalPort, asking for the same external port, and keeps the
// mapping alive until ctx ends. onChange is called with the active mapping
// and with nil when it is lost or removed. Run deletes the mapping before it
// returns.
func (c *Client) Run(ctx context.Context, internalPort int, onChange func(*Mapping)) {
	if onChange == nil {
		onChange = func(*Mapping) {}
	}
	var current *Mapping
	var owner mapper
	release := func() {
		if current == nil {
			return
		}
		removeCtx, cancel := context.WithTimeout(context.Background(), c.options.Timeout)
		if err := owner.remove(removeCtx, *current); err != nil {
			log.Printf("DynApp Shell agent: could not delete %s port mapping: %v", current.Protocol, err)
		}
		cancel()
		current, owner = nil, nil
		onChange(nil)
	}
	defer func() {
		release()
		c.setStatus(Status{State: "stopped"})
	}()
	for ctx.Err() == nil {
		var wait time.Duration
		if current != nil {
			// Renew with the protocol that granted the mapping, asking for the
			// port the router assigned.
			renewed, err := owner.add(ctx, internalPort, int(current.External.Port()), c.options.Lifetime)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("DynApp Shell agent: %s port mapping renewal failed: %v", current.Protocol, err)
				current, owner = nil, nil
				onChange(nil)
				continue
			}
			changed := renewed.External != current.External || renewed.Advertised != current.Advertised
			current = &renewed
			if changed {
				onChange(current)
			}
			c.setStatus(statusFor(renewed))
			wait = renewed.Lifetime / 2
		} else {
			granted, used, tried, err := c.acquire(ctx, internalPort)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				c.setStatus(Status{State: "unavailable", Error: err.Error(), Tried: tried})
				wait = c.options.RetryInterval
			} else {
				current, owner = &granted, used
				status := statusFor(granted)
				status.Tried = tried
				c.setStatus(status)
				onChange(current)
				wait = granted.Lifetime / 2
			}
		}
		if wait <= 0 {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func statusFor(mapping Mapping) Status {
	return Status{
		State: "mapped", Protocol: mapping.Protocol, External: mapping.External.String(),
		Internal: mapping.InternalPort, Public: mapping.Advertised,
		Expires: time.Now().Add(mapping.Lifetime).UTC().Format(time.RFC3339),
	}
}

func (c *Client) acquire(ctx context.Context, internalPort int) (Mapping, mapper, []string, error) {
	var tried []string
	var failures []error
	for _, candidate := range c.newMappers(ctx) {
		tried = append(tried, candidate.name())
		attemptCtx, cancel := context.WithTimeout(ctx, c.options.Timeout)
		mapping, err := candidate.add(attemptCtx, internalPort, internalPort, c.options.Lifetime)
		cancel()
		if err == nil {
			return mapping, candidate, tried, nil
		}
		if ctx.Err() != nil {
			return Mapping{}, nil, tried, ctx.Err()
		}
		failures = append(failures, fmt.Errorf("%s: %w", candidate.name(), err))
	}
	if len(failures) == 0 {
		return Mapping{}, nil, tried, errors.New("no port mapping protocol is available")
	}
	return Mapping{}, nil, tried, errors.Join(failures...)
}

func (c *Client) defaultMappers(ctx context.Context) []mapper {
	gatewayIP := c.options.Gateway
	if !gatewayIP.IsValid() {
		gatewayIP = discoverGateway()
	}
	var mappers []mapper
	for _, protocol := range c.options.Protocols {
		switch protocol {
		case ProtocolPCP:
			if gatewayIP.IsValid() {
				mappers = append(mappers, &pcpMapper{gateway: netip.AddrPortFrom(gatewayIP, uint16(c.options.PCPPort))})
			}
		case ProtocolNATPMP:
			if gatewayIP.IsValid() {
				mappers = append(mappers, &natpmpMapper{gateway: gatewayIP, timeout: c.options.Timeout})
			}
		case ProtocolUPnP:
			mappers = append(mappers, &upnpMapper{rootURL: c.options.UPnPRootURL, description: c.options.Description, timeout: c.options.Timeout})
		}
	}
	return mappers
}

// discoverGateway reads the default route. Android (no /proc/net/route
// access on newer releases) and other failures fall back to the .1 address of
// the interface that routes to the internet, which is the common home-router
// layout; a wrong guess only makes PCP and NAT-PMP time out.
func discoverGateway() netip.Addr {
	if ip, err := gateway.DiscoverGateway(); err == nil {
		if addr, ok := netip.AddrFromSlice(ip); ok && addr.Unmap().Is4() {
			return addr.Unmap()
		}
	}
	local := localAddressToward(netip.MustParseAddr("192.0.2.1"))
	if !local.IsValid() || !local.Is4() || !local.IsPrivate() {
		return netip.Addr{}
	}
	octets := local.As4()
	octets[3] = 1
	return netip.AddrFrom4(octets)
}

// localAddressToward returns the local address the OS would use to reach
// target. No packet is sent: connecting a UDP socket only selects a route.
func localAddressToward(target netip.Addr) netip.Addr {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(target, 9)))
	if err != nil {
		return netip.Addr{}
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}
	}
	return local.AddrPort().Addr().Unmap()
}

var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

// Advertisable reports whether an external address reported by the router is
// reachable from the internet. RFC 1918 and RFC 6598 (CGNAT) addresses mean
// another NAT sits upstream; loopback, link-local and unspecified addresses
// are never a usable public address either.
func Advertisable(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsPrivate() || addr.IsMulticast() {
		return false
	}
	return !carrierGradeNAT.Contains(addr)
}
