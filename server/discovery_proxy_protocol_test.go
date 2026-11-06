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

func TestDiscoveryProxyProtocol(t *testing.T) {
	// The sequence exercises creation, override-only updates, removal, and invalid
	// input after a valid override. All other discovery metadata stays unchanged.
	for _, source := range []string{"docker", "swarm", "kubernetes"} {
		for _, defaultRoute := range []bool{false, true} {
			name := source + "/named"
			if defaultRoute {
				name = source + "/default"
			}
			t.Run(name, func(t *testing.T) {
				routes := NewRoutes(t.Context())
				dockerWatcher := &dockerWatcherImpl{routes: routes, containerMap: map[string]*routableContainer{}}
				k8sWatcher := &K8sWatcher{routesHandler: routes}
				var previous *core.Service
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
				swarmWatcher := &dockerSwarmWatcherImpl{client: cli, routes: routes, serviceMap: map[string]*routableSwarmService{}}
				for _, step := range []struct {
					name, value string
					present     bool
					want        *bool
				}{
					{"add true", "true", true, new(true)},
					{"update false", "false", true, new(false)},
					{"remove", "", false, nil},
					{"trim and parse", " TRUE ", true, new(true)},
					{"invalid clears override", "yes", true, nil},
					{"mixed case accepted", "False", true, new(false)},
					{"empty invalid", "", true, nil},
				} {
					t.Run(step.name, func(t *testing.T) {
						delete(labels, DockerRouterLabelProxyProtocol)
						if step.present {
							labels[DockerRouterLabelProxyProtocol] = step.value
						}
						switch source {
						case "docker":
							inspect := &container.InspectResponse{
								ContainerJSONBase: &container.ContainerJSONBase{ID: "container", Name: "minecraft", State: &container.State{Running: true}},
								Config:            &container.Config{Labels: labels},
								NetworkSettings:   &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"minecraft": {IPAddress: "10.0.0.1"}}},
							}
							data, ok := dockerWatcher.parseContainerData(inspect)
							require.True(t, ok)
							desired := []*routableContainer{newRoutableContainer("10.0.0.1:25565", "mc.example.com", "container", data, nil)}
							if defaultRoute {
								desired = append(desired, newRoutableContainer("10.0.0.1:25565", "", "container", data, nil))
							}
							dockerWatcher.applyContainerRoutesLocked("container", desired)
							// Equal values at different pointer addresses must not recreate routes.
							original := dockerWatcher.containerMap["mc.example.com"]
							repeated := make([]*routableContainer, len(desired))
							for i, route := range desired {
								copy := *route
								if route.proxyProtocol != nil {
									copy.proxyProtocol = new(*route.proxyProtocol)
								}
								repeated[i] = &copy
							}
							dockerWatcher.applyContainerRoutesLocked("container", repeated)
							assert.Same(t, original, dockerWatcher.containerMap["mc.example.com"])

						case "swarm":
							services = []swarm.Service{{ID: "service", Spec: swarm.ServiceSpec{
								Annotations:  swarm.Annotations{Name: "minecraft", Labels: labels},
								Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: new(uint64(0))}},
								EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR},
							}}}
							require.NoError(t, swarmWatcher.reconcileServices(t.Context()))
							original := swarmWatcher.serviceMap["mc.example.com"]
							require.NoError(t, swarmWatcher.reconcileServices(t.Context()))
							assert.Same(t, original, swarmWatcher.serviceMap["mc.example.com"])
						case "kubernetes":
							annotations := map[string]string{AnnotationExternalServerName: "mc.example.com"}
							if defaultRoute {
								annotations = map[string]string{AnnotationDefaultServer: "true"}
							}
							if step.present {
								annotations[AnnotationProxyProtocol] = step.value
							}
							svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "minecraft", Namespace: "default", Annotations: annotations}, Spec: core.ServiceSpec{ClusterIP: "10.0.0.1"}}
							if previous == nil {
								k8sWatcher.handleAdd(svc)
							} else {
								k8sWatcher.handleUpdate(previous, svc)
							}
							previous = svc
						}
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
}
