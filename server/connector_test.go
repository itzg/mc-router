package server

import (
	"net"
	"testing"

	"github.com/pires/go-proxyproto"
	"github.com/stretchr/testify/assert"
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
