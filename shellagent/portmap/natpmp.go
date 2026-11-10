package portmap

import (
	"context"
	"net"
	"net/netip"
	"time"

	natpmp "github.com/jackpal/go-nat-pmp"
)

type natpmpMapper struct {
	gateway netip.Addr
	timeout time.Duration
}

func (m *natpmpMapper) name() string { return ProtocolNATPMP }

func (m *natpmpMapper) client(ctx context.Context) *natpmp.Client {
	timeout := m.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	return natpmp.NewClientWithTimeout(net.IP(m.gateway.AsSlice()), timeout)
}

func (m *natpmpMapper) add(ctx context.Context, internalPort, externalPort int, lifetime time.Duration) (Mapping, error) {
	client := m.client(ctx)
	address, err := client.GetExternalAddress()
	if err != nil {
		return Mapping{}, err
	}
	result, err := client.AddPortMapping("udp", internalPort, externalPort, int(lifetime/time.Second))
	if err != nil {
		return Mapping{}, err
	}
	external := netip.AddrFrom4(address.ExternalIPAddress)
	return Mapping{
		Protocol: ProtocolNATPMP, InternalPort: internalPort,
		External:   netip.AddrPortFrom(external, result.MappedExternalPort),
		Lifetime:   time.Duration(result.PortMappingLifetimeInSeconds) * time.Second,
		Advertised: Advertisable(external),
	}, nil
}

// remove deletes the mapping: lifetime 0 and suggested external port 0
// (RFC 6886 section 3.4).
func (m *natpmpMapper) remove(ctx context.Context, mapping Mapping) error {
	_, err := m.client(ctx).AddPortMapping("udp", mapping.InternalPort, 0, 0)
	return err
}
