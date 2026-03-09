package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/pires/go-proxyproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrustedProxyNetworkPolicy(t *testing.T) {
	tests := []struct {
		name           string
		trustedNets    []string
		upstreamIP     string
		expectedPolicy proxyproto.Policy
	}{
		{
			name:           "trusted IP",
			trustedNets:    []string{"10.0.0.0/8"},
			upstreamIP:     "10.0.0.1",
			expectedPolicy: proxyproto.USE,
		},
		{
			name:           "untrusted IP",
			trustedNets:    []string{"10.0.0.0/8"},
			upstreamIP:     "192.168.1.1",
			expectedPolicy: proxyproto.IGNORE,
		},
		{
			name:           "multiple trusted nets",
			trustedNets:    []string{"10.0.0.0/8", "172.16.0.0/12"},
			upstreamIP:     "172.16.0.1",
			expectedPolicy: proxyproto.USE,
		},
		{
			name:           "no trusted nets",
			trustedNets:    []string{},
			upstreamIP:     "148.184.129.202",
			expectedPolicy: proxyproto.USE,
		},
		{
			name:           "remote trusted IP",
			trustedNets:    []string{"203.0.113.0/24"},
			upstreamIP:     "203.0.113.10",
			expectedPolicy: proxyproto.USE,
		},
		{
			name:           "remote untrusted IP",
			trustedNets:    []string{"203.0.113.0/24"},
			upstreamIP:     "198.51.100.1",
			expectedPolicy: proxyproto.IGNORE,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &Connector{
				trustedProxyNets: parseTrustedProxyNets(test.trustedNets),
			}

			policy := c.createProxyProtoPolicy()
			upstreamAddr := &net.TCPAddr{IP: net.ParseIP(test.upstreamIP)}
			policyResult, _ := policy(proxyproto.ConnPolicyOptions{
				Upstream: upstreamAddr,
			})
			assert.Equal(t, test.expectedPolicy, policyResult, "Unexpected policy result for %s", test.name)
		})
	}
}

func parseTrustedProxyNets(nets []string) []*net.IPNet {
	parsedNets := make([]*net.IPNet, 0, len(nets))
	for _, n := range nets {
		_, ipNet, _ := net.ParseCIDR(n)
		parsedNets = append(parsedNets, ipNet)
	}
	return parsedNets
}

func TestConnectorWakeTracking(t *testing.T) {
	c := &Connector{wakingServers: NewActiveConnections()}

	assert.False(t, c.isWakeInProgress("scale-target"))
	c.wakingServers.Increment("scale-target")
	assert.True(t, c.isWakeInProgress("scale-target"))

	// track concurrent wake operations for same route
	c.wakingServers.Increment("scale-target")
	c.wakingServers.Decrement("scale-target")
	assert.True(t, c.isWakeInProgress("scale-target"))

	c.wakingServers.Decrement("scale-target")
	assert.False(t, c.isWakeInProgress("scale-target"))
}

func TestConnectorGetLoadingMOTD(t *testing.T) {
	oldRoutes := Routes
	defer func() {
		Routes = oldRoutes
	}()

	Routes = NewRoutes()
	Routes.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "route loading")

	c := &Connector{loadingMOTD: "global loading"}
	assert.Equal(t, "route loading", c.getLoadingMOTD("mc.example.com"))
	assert.Equal(t, "global loading", c.getLoadingMOTD("other.example.com"))

	Routes.SetDefaultRoute("default:25565", "", nil, nil, "", "default loading")
	assert.Equal(t, "default loading", c.getLoadingMOTD(""))
}

func writeTestPacket(w io.Writer, packetID int32, payload func(w io.Writer)) error {
	var b bytes.Buffer
	_ = mcproto.WriteVarInt(&b, packetID)
	if payload != nil {
		payload(&b)
	}

	var framed bytes.Buffer
	_ = mcproto.WriteVarInt(&framed, int32(b.Len()))
	framed.Write(b.Bytes())
	_, err := w.Write(framed.Bytes())
	return err
}

func TestConnectorMOTDFallback(t *testing.T) {
	oldRoutes := Routes
	defer func() {
		Routes = oldRoutes
	}()

	Routes = NewRoutes()

	backendAddress := "127.0.0.1:0"

	scaleUpCalled := false
	waker := func(ctx context.Context) (string, error) {
		scaleUpCalled = true
		return backendAddress, nil
	}

	Routes.CreateMapping("mc.example.com", backendAddress, "", waker, nil, "fallback asleep", "fallback loading")

	metricsBuilder := discardMetricsBuilder{}
	c := NewConnector(context.Background(), metricsBuilder.BuildConnectorMetrics(), false, false, nil)
	c.UseAsleepMOTD("global asleep")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go c.acceptConnections(ln, 100, 0)

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer clientConn.Close()

	err = writeTestPacket(clientConn, 0x00, func(w io.Writer) {
		_ = mcproto.WriteVarInt(w, 758)
		_ = mcproto.WriteString(w, "mc.example.com")
		w.Write([]byte{0x63, 0xdd})
		_ = mcproto.WriteVarInt(w, 1)
	})
	require.NoError(t, err)

	// 2. Send Status Request
	err = writeTestPacket(clientConn, 0x00, nil)
	require.NoError(t, err)

	// 3. Read Status Response
	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(clientConn)

	frame, err := mcproto.ReadFrame(reader, clientConn.RemoteAddr())
	require.NoError(t, err)

	packetReader := bytes.NewReader(frame.Payload)
	packetID, err := mcproto.ReadVarInt(packetReader)
	require.NoError(t, err)
	assert.Equal(t, 0x00, packetID)

	jsonStr, err := mcproto.ReadString(packetReader)
	require.NoError(t, err)

	assert.Contains(t, jsonStr, "fallback asleep")
	assert.False(t, scaleUpCalled, "Waker should NOT be called for a status request")
}
