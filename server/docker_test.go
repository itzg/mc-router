package server

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerEndpointIP(t *testing.T) {
	t.Run("prefers IPv4 when both exist", func(t *testing.T) {
		endpoint := &network.EndpointSettings{
			IPAddress:         "10.0.0.10",
			GlobalIPv6Address: "fd00::10",
		}
		assert.Equal(t, "10.0.0.10", dockerEndpointIP(endpoint))
	})

	t.Run("falls back to IPv6 when IPv4 absent", func(t *testing.T) {
		endpoint := &network.EndpointSettings{
			GlobalIPv6Address: "fd00::20",
		}
		assert.Equal(t, "fd00::20", dockerEndpointIP(endpoint))
	})

	t.Run("returns empty when endpoint missing", func(t *testing.T) {
		assert.Empty(t, dockerEndpointIP(nil))
	})
}

func TestDockerBackendEndpoint(t *testing.T) {
	assert.Equal(t, "10.0.0.10:25565", dockerBackendEndpoint("10.0.0.10", 25565))
	assert.Equal(t, "[fd00::20]:25565", dockerBackendEndpoint("fd00::20", 25565))
}

func TestDockerWatcherParseContainerData_IPSelection(t *testing.T) {
	makeInspect := func(labels map[string]string, networks map[string]*network.EndpointSettings) *container.InspectResponse {
		return &container.InspectResponse{
			Config: &container.Config{
				Labels: labels,
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: networks,
			},
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:   "abc123",
				Name: "example",
				State: &container.State{
					Running: true,
				},
			},
		}
	}

	w := &dockerWatcherImpl{}

	t.Run("uses ipv4 for single-network container", func(t *testing.T) {
		inspect := makeInspect(
			map[string]string{
				DockerRouterLabelHost: "mc.example.com",
			},
			map[string]*network.EndpointSettings{
				"v6net": {
					IPAddress:         "172.20.0.2",
					GlobalIPv6Address: "fd00::2",
				},
			},
		)

		data, ok := w.parseContainerData(inspect)
		require.True(t, ok)
		assert.Equal(t, "172.20.0.2", data.ip)
		assert.Equal(t, uint64(25565), data.port)
	})

	t.Run("uses ipv6 for single-network container when ipv4 absent", func(t *testing.T) {
		inspect := makeInspect(
			map[string]string{
				DockerRouterLabelHost: "mc.example.com",
			},
			map[string]*network.EndpointSettings{
				"v6net": {
					GlobalIPv6Address: "fd00::2",
				},
			},
		)

		data, ok := w.parseContainerData(inspect)
		require.True(t, ok)
		assert.Equal(t, "fd00::2", data.ip)
	})

	t.Run("uses ipv6 for selected network when network label is set", func(t *testing.T) {
		inspect := makeInspect(
			map[string]string{
				DockerRouterLabelHost:    "mc.example.com",
				DockerRouterLabelNetwork: "v6net",
			},
			map[string]*network.EndpointSettings{
				"other": {
					IPAddress: "172.20.0.5",
				},
				"v6net": {
					GlobalIPv6Address: "fd00::6",
				},
			},
		)

		data, ok := w.parseContainerData(inspect)
		require.True(t, ok)
		assert.Equal(t, "fd00::6", data.ip)
	})

	t.Run("rejects container with no routable ip while running", func(t *testing.T) {
		inspect := makeInspect(
			map[string]string{
				DockerRouterLabelHost: "mc.example.com",
			},
			map[string]*network.EndpointSettings{
				"v6net": {},
			},
		)

		_, ok := w.parseContainerData(inspect)
		assert.False(t, ok)
	})
}
