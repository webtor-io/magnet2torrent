package main

import (
	"errors"
	"net"
	"net/netip"
	"syscall"

	log "github.com/sirupsen/logrus"
)

// The addresses a tracker is dialled at come from the request: a magnet's
// tr=, or the DNS answer for its host. Without a filter a magnet makes the
// pod connect wherever it says: to itself, into the cluster, to the node's
// network, to the metadata endpoint at 169.254.169.254. torrent-web-seeder
// keeps its own copy on purpose: too small to be worth a shared module.

const (
	allowLocalNetworksFlag = "allow-local-networks"
	allowLocalNetworksEnv  = "ALLOW_LOCAL_NETWORKS"
)

const refusedLocalNetworkMsg = "refused connection to a local network address (allow with --" +
	allowLocalNetworksFlag + " or " + allowLocalNetworksEnv + "=true)"

var errLocalNetwork = errors.New("local network address refused")

// cgnat is the shared address space (RFC 6598): not public, and not covered
// by the netip predicates.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLocalNetworkAddr reports whether a connection to ip would stay on this
// host or its networks rather than reach the internet.
func isLocalNetworkAddr(ip netip.Addr) bool {
	// ::ffff:a.b.c.d reaches a.b.c.d, and IsUnspecified and Prefix.Contains
	// do not unmap on their own; nor does IsUnspecified ignore a zone.
	ip = ip.Unmap().WithZone("")
	// An unset address is this host to the kernel; the hooks below also map
	// what they cannot parse to it.
	return !ip.IsValid() ||
		ip.IsLoopback() ||
		ip.IsPrivate() || // RFC 1918 and IPv6 ULA (fc00::/7)
		ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		cgnat.Contains(ip)
}

func checkLocalNetwork(ip netip.Addr, addr string) error {
	if !isLocalNetworkAddr(ip) {
		return nil
	}
	log.WithField("addr", addr).Warn(refusedLocalNetworkMsg)
	return errLocalNetwork
}

// refuseLocalNetworks is a net.Dialer Control hook. It runs after name
// resolution, right before connect, so it judges the address actually
// dialled: every address a host name resolves to, and the target of every
// HTTP redirect.
func refuseLocalNetworks(_, address string, _ syscall.RawConn) error {
	// One that does not parse comes out as the zero Addr, which is refused.
	ap, _ := netip.ParseAddrPort(address)
	return checkLocalNetwork(ap.Addr(), address)
}

// localNetworkRefusingConn is the UDP counterpart. A UDP tracker socket is
// unconnected: the client resolves the tracker and sends with WriteTo, so a
// Control hook (which sees only the local address) cannot judge it.
type localNetworkRefusingConn struct{ net.PacketConn }

func (c localNetworkRefusingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// Any other address type yields the zero Addr, which is refused.
	ua, _ := addr.(*net.UDPAddr)
	if err := checkLocalNetwork(ua.AddrPort().Addr(), ua.String()); err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(p, addr)
}

func listenPacketRefusingLocalNetworks(network, addr string) (net.PacketConn, error) {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	return localNetworkRefusingConn{pc}, nil
}
