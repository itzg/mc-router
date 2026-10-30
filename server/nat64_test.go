package server

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/pires/go-proxyproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNAT64Prefixes(t *testing.T) {
	tests := []struct {
		name    string
		cidrs   []string
		wantErr string
	}{
		{name: "well-known prefix", cidrs: []string{"64:ff9b::/96"}},
		{name: "local-use prefix", cidrs: []string{"64:ff9b:1::/48"}},
		{name: "local-use /96 prefix", cidrs: []string{"64:ff9b:1::/96"}},
		{name: "every RFC 6052 length", cidrs: []string{"2001:db8::/32", "2001:db8:100::/40", "2001:db8:122::/48", "2001:db8:122:300::/56", "2001:db8:122:344::/64", "2001:db8:122:344::/96"}},
		{name: "not a CIDR", cidrs: []string{"64:ff9b::"}, wantErr: "could not parse"},
		{name: "IPv4 prefix", cidrs: []string{"192.0.2.0/24"}, wantErr: "not an IPv6 prefix"},
		{name: "IPv4-mapped prefix", cidrs: []string{"::ffff:0.0.0.0/96"}, wantErr: "not an IPv6 prefix"},
		{name: "unsupported length", cidrs: []string{"64:ff9b::/104"}, wantErr: "must have a length of"},
		{name: "host bits set", cidrs: []string{"64:ff9b::1/96"}, wantErr: "bits set beyond"},
		{name: "reserved bits set", cidrs: []string{"64:ff9b:0:0:100::/96"}, wantErr: "bits 64 to 71"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefixes, err := ParseNAT64Prefixes(test.cidrs)
			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, prefixes, len(test.cidrs))
		})
	}
}

func TestNAT64PrefixesExtractIPv4(t *testing.T) {
	tests := []struct {
		name     string
		prefixes string
		addr     string
		want     string
	}{
		// Examples from RFC 6052 section 2.4, all embedding 192.0.2.33
		{name: "RFC 6052 /32", prefixes: "2001:db8::/32", addr: "2001:db8:c000:221::", want: "192.0.2.33"},
		{name: "RFC 6052 /40", prefixes: "2001:db8:100::/40", addr: "2001:db8:1c0:2:21::", want: "192.0.2.33"},
		{name: "RFC 6052 /48", prefixes: "2001:db8:122::/48", addr: "2001:db8:122:c000:2:2100::", want: "192.0.2.33"},
		{name: "RFC 6052 /56", prefixes: "2001:db8:122:300::/56", addr: "2001:db8:122:3c0:0:221::", want: "192.0.2.33"},
		{name: "RFC 6052 /64", prefixes: "2001:db8:122:344::/64", addr: "2001:db8:122:344:c0:2:2100:0", want: "192.0.2.33"},
		{name: "RFC 6052 /96", prefixes: "2001:db8:122:344::/96", addr: "2001:db8:122:344::192.0.2.33", want: "192.0.2.33"},
		{name: "well-known prefix", prefixes: "64:ff9b::/96", addr: "64:ff9b::192.0.2.33", want: "192.0.2.33"},
		{name: "RFC 8215 local-use /48", prefixes: "64:ff9b:1::/48", addr: "64:ff9b:1:c000:2:2100::", want: "192.0.2.33"},
		{name: "RFC 8215 local-use /96", prefixes: "64:ff9b:1::/96", addr: "64:ff9b:1::c000:221", want: "192.0.2.33"},
		{name: "longest prefix, short listed first", prefixes: "64:ff9b:1::/48,64:ff9b:1::/96", addr: "64:ff9b:1::c000:221", want: "192.0.2.33"},
		{name: "longest prefix, long listed first", prefixes: "64:ff9b:1::/96,64:ff9b:1::/48", addr: "64:ff9b:1::c000:221", want: "192.0.2.33"},
		{name: "only within shorter prefix", prefixes: "64:ff9b:1::/96,64:ff9b:1::/48", addr: "64:ff9b:1:c000:2:2100::", want: "192.0.2.33"},
		{name: "outside prefix", prefixes: "64:ff9b::/96", addr: "2001:db8::1"},
		{name: "reserved bits set", prefixes: "2001:db8:122:344::/64", addr: "2001:db8:122:344:1c0:2:2100:0"},
		{name: "IPv4 address", prefixes: "64:ff9b::/96", addr: "192.0.2.33"},
		{name: "IPv4-mapped address", prefixes: "64:ff9b::/96", addr: "::ffff:192.0.2.33"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefixes, err := ParseNAT64Prefixes(strings.Split(test.prefixes, ","))
			require.NoError(t, err)

			got, ok := prefixes.ExtractIPv4(netip.MustParseAddr(test.addr))
			if test.want == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, netip.MustParseAddr(test.want), got)
		})
	}
}

func TestNAT64PrefixesRecoverIPv4Addr(t *testing.T) {
	prefixes, err := ParseNAT64Prefixes([]string{"64:ff9b::/96", "64:ff9b:1::/96"})
	require.NoError(t, err)

	tests := []struct {
		name string
		addr net.Addr
		want string
	}{
		{name: "first prefix", addr: tcpAddr(t, "[64:ff9b::c000:221]:12345"), want: "192.0.2.33:12345"},
		{name: "second prefix", addr: tcpAddr(t, "[64:ff9b:1::cb00:7105]:54321"), want: "203.0.113.5:54321"},
		{name: "native IPv6 client", addr: tcpAddr(t, "[2001:db8::1]:12345"), want: "[2001:db8::1]:12345"},
		{name: "native IPv4 client", addr: tcpAddr(t, "198.51.100.7:12345"), want: "198.51.100.7:12345"},
		{name: "not a TCP address", addr: &net.UnixAddr{Name: "/tmp/socket", Net: "unix"}, want: "/tmp/socket"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := prefixes.RecoverIPv4Addr(test.addr)
			assert.Equal(t, test.want, got.String())
		})
	}
}

func TestNAT64PrefixesProxyHeader(t *testing.T) {
	prefixes, err := ParseNAT64Prefixes([]string{"64:ff9b::/96"})
	require.NoError(t, err)

	t.Run("rewrites NAT64 source and drops CRC32C", func(t *testing.T) {
		header := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv6,
			SourceAddr: tcpAddr(t, "[64:ff9b::c000:221]:12345"), DestinationAddr: tcpAddr(t, "[2001:db8::21]:25565")}
		uniqueID := proxyproto.TLV{Type: proxyproto.PP2_TYPE_UNIQUE_ID, Value: []byte("connection-id")}
		require.NoError(t, header.SetTLVs([]proxyproto.TLV{{Type: proxyproto.PP2_TYPE_CRC32C, Value: []byte{1, 2, 3, 4}}, uniqueID}))

		got := prefixes.proxyHeader(header)

		assert.Equal(t, proxyproto.TCPv4, got.TransportProtocol)
		assert.Equal(t, "192.0.2.33:12345", got.SourceAddr.String())
		assert.Equal(t, "0.0.0.0:25565", got.DestinationAddr.String())
		tlvs, err := got.TLVs()
		require.NoError(t, err)
		assert.Equal(t, []proxyproto.TLV{uniqueID}, tlvs)

		assert.Equal(t, proxyproto.TCPv6, header.TransportProtocol, "original header must not be modified")
		originalTLVs, err := header.TLVs()
		require.NoError(t, err)
		assert.Len(t, originalTLVs, 2, "original header must not be modified")
	})

	t.Run("keeps native IPv6 source", func(t *testing.T) {
		header := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv6,
			SourceAddr: tcpAddr(t, "[2001:db8::1]:12345"), DestinationAddr: tcpAddr(t, "[2001:db8::21]:25565")}
		assert.Same(t, header, prefixes.proxyHeader(header))
	})
}

func tcpAddr(t *testing.T, addr string) *net.TCPAddr {
	t.Helper()
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr))
}
