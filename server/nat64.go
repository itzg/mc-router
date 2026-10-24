package server

import (
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/pires/go-proxyproto"
)

// NAT64Prefixes are the prefixes a NAT64 translator uses to embed a client's IPv4 address
// in the IPv6 source address of a connection, as described by RFC 6052 section 2.2.
type NAT64Prefixes []netip.Prefix

// ParseNAT64Prefixes parses prefixes in CIDR notation. The length sets where the IPv4 address
// is embedded, so it must match the translator's configuration.
func ParseNAT64Prefixes(cidrs []string) (NAT64Prefixes, error) {
	prefixes := make(NAT64Prefixes, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("could not parse NAT64 prefix %q: %w", cidr, err)
		}
		if !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("NAT64 prefix %q is not an IPv6 prefix", cidr)
		}
		switch prefix.Bits() {
		case 32, 40, 48, 56, 64, 96:
		default:
			return nil, fmt.Errorf("NAT64 prefix %q must have a length of /32, /40, /48, /56, /64 or /96", cidr)
		}
		if prefix.Masked() != prefix {
			return nil, fmt.Errorf("NAT64 prefix %q has bits set beyond its prefix length", cidr)
		}
		// RFC 6052 reserves bits 64 to 71. Only a /96 prefix includes them.
		if prefix.Addr().As16()[8] != 0 {
			return nil, fmt.Errorf("NAT64 prefix %q must have bits 64 to 71 set to zero", cidr)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// ExtractIPv4 returns the IPv4 address embedded in addr using the most specific matching prefix.
func (p NAT64Prefixes) ExtractIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() || addr.Is4In6() {
		return netip.Addr{}, false
	}
	var matched netip.Prefix
	for _, prefix := range p {
		if prefix.Contains(addr) && (!matched.IsValid() || prefix.Bits() > matched.Bits()) {
			matched = prefix
		}
	}
	if !matched.IsValid() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	if b[8] != 0 {
		// RFC 6052 requires the reserved bits 64 to 71 to be zero
		return netip.Addr{}, false
	}
	// The IPv4 address follows the prefix and skips the reserved byte
	var v4 [4]byte
	i := matched.Bits() / 8
	for n := range v4 {
		if i == 8 {
			i++
		}
		v4[n] = b[i]
		i++
	}
	return netip.AddrFrom4(v4), true
}

// RecoverIPv4Addr returns the IPv4 TCP address embedded in addr and true. If addr isn't a TCP
// address within the prefixes, it returns addr and false.
func (p NAT64Prefixes) RecoverIPv4Addr(addr net.Addr) (net.Addr, bool) {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr, false
	}
	addrPort := tcpAddr.AddrPort()
	v4, ok := p.ExtractIPv4(addrPort.Addr())
	if !ok {
		return addr, false
	}
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(v4, addrPort.Port())), true
}

// proxyHeader returns a copy of header that uses the recovered IPv4 source address. If there
// is no address to recover, it returns header.
func (p NAT64Prefixes) proxyHeader(header *proxyproto.Header) *proxyproto.Header {
	src, ok := p.RecoverIPv4Addr(header.SourceAddr)
	if !ok {
		return header
	}
	// A PROXY header uses one address family for both addresses, and the IPv6 destination
	// has no IPv4 form. Send 0.0.0.0 with the original port.
	dest := &net.TCPAddr{IP: net.IPv4zero}
	if tcpAddr, ok := header.DestinationAddr.(*net.TCPAddr); ok {
		dest.Port = tcpAddr.Port
	}
	rewritten := *header
	rewritten.SourceAddr = src
	rewritten.DestinationAddr = dest
	rewritten.TransportProtocol = proxyproto.TCPv4
	// A CRC32C TLV covers the original addresses, so drop it. Malformed TLVs stay as received.
	// SetTLVs fails only on values that are too long, which a parsed header can't contain.
	if tlvs, err := header.TLVs(); err == nil {
		_ = rewritten.SetTLVs(slices.DeleteFunc(tlvs, func(tlv proxyproto.TLV) bool {
			return tlv.Type == proxyproto.PP2_TYPE_CRC32C
		}))
	}
	return &rewritten
}
