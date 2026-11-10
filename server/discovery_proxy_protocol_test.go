package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDockerDiscoveryProxyProtocol(t *testing.T) {
	// The sequence exercises creation, override-only updates, removal, and invalid
	// input after a valid override. All other discovery metadata stays unchanged.
	for _, defaultRoute := range []bool{false, true} {
		name := "named"
		if defaultRoute {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			watcher := &dockerWatcherImpl{
				routes:       routes,
				containerMap: map[string]*routableContainer{},
			}
			labels := map[string]string{DockerRouterLabelHost: "mc.example.com"}
			if defaultRoute {
				labels[DockerRouterLabelDefault] = "true"
			}

			for _, step := range []struct {
				name    string
				value   string
				present bool
				want    *bool
			}{
				{
					name:    "add true",
					value:   "true",
					present: true,
					want:    new(true),
				},
				{
					name:    "update false",
					value:   "false",
					present: true,
					want:    new(false),
				},
				{
					name: "remove",
				},
				{
					name:    "trim and parse",
					value:   " TRUE ",
					present: true,
					want:    new(true),
				},
				{
					name:    "invalid clears override",
					value:   "yes",
					present: true,
				},
				{
					name:    "mixed case accepted",
					value:   "False",
					present: true,
					want:    new(false),
				},
				{
					name:    "empty invalid",
					value:   "",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					delete(labels, DockerRouterLabelProxyProtocol)
					if step.present {
						labels[DockerRouterLabelProxyProtocol] = step.value
					}
					inspect := &container.InspectResponse{
						ContainerJSONBase: &container.ContainerJSONBase{
							ID:    "container",
							Name:  "minecraft",
							State: &container.State{Running: true},
						},
						Config: &container.Config{Labels: labels},
						NetworkSettings: &container.NetworkSettings{
							Networks: map[string]*network.EndpointSettings{"minecraft": {IPAddress: "10.0.0.1"}},
						},
					}
					data, ok := watcher.parseContainerData(inspect)
					require.True(t, ok)
					desired := []*routableContainer{newRoutableContainer("10.0.0.1:25565", "mc.example.com", "container", data, nil)}
					if defaultRoute {
						desired = append(desired, newRoutableContainer("10.0.0.1:25565", "", "container", data, nil))
					}
					watcher.applyContainerRoutesLocked("container", desired)

					// Equal values at different pointer addresses must not recreate routes.
					original := watcher.containerMap["mc.example.com"]
					repeated := make([]*routableContainer, len(desired))
					for i, route := range desired {
						copy := *route
						if route.proxyProtocol != nil {
							copy.proxyProtocol = new(*route.proxyProtocol)
						}
						repeated[i] = &copy
					}
					watcher.applyContainerRoutesLocked("container", repeated)
					assert.Same(t, original, watcher.containerMap["mc.example.com"])

					host := "mc.example.com"
					if defaultRoute {
						host = "unmatched.example.com"
					}
					assert.Equal(t, step.want, routes.GetProxyProtocol(host))

				})
			}
		})
	}
}

func TestSwarmDiscoveryProxyProtocol(t *testing.T) {
	// The sequence exercises creation, override-only updates, removal, and invalid
	// input after a valid override. All other discovery metadata stays unchanged.
	for _, defaultRoute := range []bool{false, true} {
		name := "named"
		if defaultRoute {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			labels := map[string]string{DockerRouterLabelHost: "mc.example.com"}
			if defaultRoute {
				labels[DockerRouterLabelDefault] = "true"
			}
			var services []swarm.Service
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/services"):
					_ = json.NewEncoder(w).Encode(services)
				case strings.HasSuffix(r.URL.Path, "/version"):
					_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.47"})
				case strings.HasSuffix(r.URL.Path, "/networks"):
					_ = json.NewEncoder(w).Encode([]network.Inspect{})
				default:
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(api.URL), client.WithVersion("1.47"))
			require.NoError(t, err)
			defer cli.Close()
			watcher := &dockerSwarmWatcherImpl{
				client:     cli,
				routes:     routes,
				serviceMap: map[string]*routableSwarmService{},
			}

			for _, step := range []struct {
				name    string
				value   string
				present bool
				want    *bool
			}{
				{
					name:    "add true",
					value:   "true",
					present: true,
					want:    new(true),
				},
				{
					name:    "update false",
					value:   "false",
					present: true,
					want:    new(false),
				},
				{
					name: "remove",
				},
				{
					name:    "trim and parse",
					value:   " TRUE ",
					present: true,
					want:    new(true),
				},
				{
					name:    "invalid clears override",
					value:   "yes",
					present: true,
				},
				{
					name:    "mixed case accepted",
					value:   "False",
					present: true,
					want:    new(false),
				},
				{
					name:    "empty invalid",
					value:   "",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					delete(labels, DockerRouterLabelProxyProtocol)
					if step.present {
						labels[DockerRouterLabelProxyProtocol] = step.value
					}
					services = []swarm.Service{{
						ID: "service",
						Spec: swarm.ServiceSpec{
							Annotations:  swarm.Annotations{Name: "minecraft", Labels: labels},
							Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: new(uint64(0))}},
							EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR},
						},
					}}
					require.NoError(t, watcher.reconcileServices(t.Context()))
					original := watcher.serviceMap["mc.example.com"]
					require.NoError(t, watcher.reconcileServices(t.Context()))
					assert.Same(t, original, watcher.serviceMap["mc.example.com"])

					host := "mc.example.com"
					if defaultRoute {
						host = "unmatched.example.com"
					}
					assert.Equal(t, step.want, routes.GetProxyProtocol(host))

				})
			}
		})
	}
}

func TestKubernetesDiscoveryProxyProtocol(t *testing.T) {
	// The sequence exercises creation, override-only updates, removal, and invalid
	// input after a valid override. All other discovery metadata stays unchanged.
	for _, defaultRoute := range []bool{false, true} {
		name := "named"
		if defaultRoute {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			watcher := &K8sWatcher{routesHandler: routes}
			var previous *core.Service

			for _, step := range []struct {
				name    string
				value   string
				present bool
				want    *bool
			}{
				{
					name:    "add true",
					value:   "true",
					present: true,
					want:    new(true),
				},
				{
					name:    "update false",
					value:   "false",
					present: true,
					want:    new(false),
				},
				{
					name: "remove",
				},
				{
					name:    "trim and parse",
					value:   " TRUE ",
					present: true,
					want:    new(true),
				},
				{
					name:    "invalid clears override",
					value:   "yes",
					present: true,
				},
				{
					name:    "mixed case accepted",
					value:   "False",
					present: true,
					want:    new(false),
				},
				{
					name:    "empty invalid",
					value:   "",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					annotations := map[string]string{AnnotationExternalServerName: "mc.example.com"}
					if defaultRoute {
						annotations = map[string]string{AnnotationDefaultServer: "true"}
					}
					if step.present {
						annotations[AnnotationProxyProtocol] = step.value
					}
					svc := &core.Service{
						ObjectMeta: metav1.ObjectMeta{
							Name:        "minecraft",
							Namespace:   "default",
							Annotations: annotations,
						},
						Spec: core.ServiceSpec{ClusterIP: "10.0.0.1"},
					}
					if previous == nil {
						watcher.handleAdd(svc)
					} else {
						watcher.handleUpdate(previous, svc)
					}
					previous = svc

					host := "mc.example.com"
					if defaultRoute {
						host = "unmatched.example.com"
					}
					assert.Equal(t, step.want, routes.GetProxyProtocol(host))

				})
			}
		})
	}
}
