package main

import (
	"errors"
	"net"
	"net/netip"
	"syscall"

	log "github.com/sirupsen/logrus"
)

// The addresses a tracker is dialled at come from the request: a magnet's
// tr=, or the DNS answer for its host; the same goes for a metainfo source
// (xs=, as=), fetched with a GET. Without a filter a magnet makes the
// pod connect wherever it says: to itself, into the cluster, to the metadata
// endpoint at 169.254.169.254. The filter knows address classes, not this
// deployment's addresses: a node's public IP, and whatever the node or a
// host-network pod serves on it, is a public address and passes. Keeping
// those out takes an egress NetworkPolicy. torrent-web-seeder keeps its own
// copy on purpose: too small to be worth a shared module.

const (
	allowLocalNetworksFlag = "allow-local-networks"
	allowLocalNetworksEnv  = "ALLOW_LOCAL_NETWORKS"
)

const refusedLocalNetworkMsg = "refused connection to a local network address (allow with --" +
	allowLocalNetworksFlag + " or " + allowLocalNetworksEnv + "=true; logged at most once a minute)"

var errLocalNetwork = errors.New("local network address refused")

// localPrefixes are the non-public ranges the netip predicates do not cover.
// None is routed on the internet, so a route to one, if any, is a local one.
var localPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space, CGNAT (RFC 6598)
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network" (RFC 1122)
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, and 255.255.255.255 broadcast
	netip.MustParsePrefix("fec0::/10"),     // site-local, deprecated (RFC 3879)
}

// ipv4In6 are the IPv6 forms that carry an IPv4 address, with the offset of
// its 4 bytes: a NAT64 gateway, a 6to4 relay or a dual stack delivers the
// packet to that IPv4 address, so it is the one to judge.
var ipv4In6 = []struct {
	prefix netip.Prefix
	at     int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},   // NAT64 well-known prefix (RFC 6052)
	{netip.MustParsePrefix("64:ff9b:1::/48"), 12}, // NAT64 local-use prefix (RFC 8215), as a /96
	{netip.MustParsePrefix("2002::/16"), 2},       // 6to4 (RFC 3056)
	{netip.MustParsePrefix("::/96"), 12},          // IPv4-compatible (RFC 4291, deprecated)
}

// isLocalNetworkAddr reports whether a connection to ip would stay on this
// host or its networks rather than reach the internet.
func isLocalNetworkAddr(ip netip.Addr) bool {
	// ::ffff:a.b.c.d reaches a.b.c.d, and IsUnspecified and Prefix.Contains
	// do not unmap on their own; nor does IsUnspecified ignore a zone, and
	// Prefix.Contains never matches a zoned address.
	ip = ip.Unmap().WithZone("")
	// An unset address is this host to the kernel; the hooks below also map
	// what they cannot parse to it.
	if !ip.IsValid() ||
		ip.IsLoopback() ||
		ip.IsPrivate() || // RFC 1918 and IPv6 ULA (fc00::/7)
		ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return true
	}
	for _, p := range localPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	// After the checks above, so that :: and ::1, inside ::/96, are judged as
	// what they are. The IPv4 address matches no IPv6 prefix: one level deep.
	for _, e := range ipv4In6 {
		if e.prefix.Contains(ip) {
			b := ip.As16()
			return isLocalNetworkAddr(netip.AddrFrom4([4]byte(b[e.at : e.at+4])))
		}
	}
	return false
}

// checkLocalNetwork refuses ip if it is on a local network. Each refused
// tracker is refused again on every announce, a magnet can name many, and
// each request has its own client: the warning is limited per server, which
// is one per process, and m2t_local_network_refusals_total counts them all.
func (s *server) checkLocalNetwork(ip netip.Addr, addr string) error {
	if !isLocalNetworkAddr(ip) {
		return nil
	}
	localNetworkRefusals.Inc()
	if s.refusalWarn.Allow() {
		log.WithField("addr", addr).Warn(refusedLocalNetworkMsg)
	}
	return errLocalNetwork
}

// refuseLocalNetworks is a net.Dialer Control hook. It runs after name
// resolution, right before connect, so it judges the address actually
// dialled: every address a host name resolves to, and the target of every
// HTTP redirect.
func (s *server) refuseLocalNetworks(_, address string, _ syscall.RawConn) error {
	// One that does not parse comes out as the zero Addr, which is refused.
	ap, _ := netip.ParseAddrPort(address)
	return s.checkLocalNetwork(ap.Addr(), address)
}

// localNetworkRefusingConn is the UDP counterpart. A UDP tracker socket is
// unconnected: the client resolves the tracker and sends with WriteTo, so a
// Control hook (which sees only the local address) cannot judge it.
type localNetworkRefusingConn struct {
	net.PacketConn
	s *server
}

func (c localNetworkRefusingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// Any other address type yields the zero Addr, which is refused.
	ua, _ := addr.(*net.UDPAddr)
	if err := c.s.checkLocalNetwork(ua.AddrPort().Addr(), ua.String()); err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (s *server) listenPacketRefusingLocalNetworks(network, addr string) (net.PacketConn, error) {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	return localNetworkRefusingConn{pc, s}, nil
}
