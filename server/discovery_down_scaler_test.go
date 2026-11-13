package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDockerDiscoveryScaleDownDelay(t *testing.T) {
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
				config:       dockerWatcherConfig{autoScaleDown: true},
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
				want    *time.Duration
			}{
				{name: "inherit"},
				{
					name:    "add",
					value:   "12h",
					present: true,
					want:    new(12 * time.Hour),
				},
				{
					name:    "timeout-only update",
					value:   "5m",
					present: true,
					want:    new(5 * time.Minute),
				},
				{name: "remove"},
				{
					name:    "trim",
					value:   " 30s ",
					present: true,
					want:    new(30 * time.Second),
				},
				{
					name:    "invalid clears override",
					value:   "forever",
					present: true,
				},
				{
					name:    "restore",
					value:   "1h",
					present: true,
					want:    new(time.Hour),
				},
				{
					name:    "zero",
					value:   "0s",
					present: true,
				},
				{
					name:    "negative",
					value:   "-1h",
					present: true,
				},
				{
					name:    "empty",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					delete(labels, DockerRouterLabelAutoScaleDownAfter)
					if step.present {
						labels[DockerRouterLabelAutoScaleDownAfter] = step.value
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
					// Startup list conversion and incremental inspect conversion both
					// need to preserve the override, including default-route aliases.
					startup := newRoutableContainer("10.0.0.1:25565", "mc.example.com", "container", data, NewDockerScalingTarget("container", "minecraft"))
					assert.Equal(t, step.want, startup.autoScaleDownAfter)
					api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if !strings.HasSuffix(r.URL.Path, "/containers/container/json") {
							http.NotFound(w, r)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(inspect)
					}))
					defer api.Close()
					cli, err := client.NewClientWithOpts(client.WithHost(api.URL), client.WithVersion("1.47"))
					require.NoError(t, err)
					defer cli.Close()
					watcher.client = cli
					desired, err := watcher.containersForID(t.Context(), "container")
					require.NoError(t, err)
					require.Len(t, desired, map[bool]int{false: 1, true: 2}[defaultRoute])
					watcher.applyContainerRoutesLocked("container", desired)

					// Equal values at different pointer addresses must not recreate routes.
					original := watcher.containerMap["mc.example.com"]
					repeated := make([]*routableContainer, len(desired))
					for i, route := range desired {
						copy := *route
						if route.autoScaleDownAfter != nil {
							copy.autoScaleDownAfter = new(*route.autoScaleDownAfter)
						}
						repeated[i] = &copy
					}
					watcher.applyContainerRoutesLocked("container", repeated)
					assert.Same(t, original, watcher.containerMap["mc.example.com"])

					host := "mc.example.com"
					if defaultRoute {
						host = "unmatched.example.com"
					}
					_, _, target, _, sleeper, _ := routes.FindBackendForServerAddress(t.Context(), host)
					require.NotNil(t, sleeper)
					want := 10 * time.Minute
					if step.want != nil {
						want = *step.want
					}
					assert.Equal(t, want, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

				})
			}
		})
	}
}

func TestSwarmDiscoveryScaleDownDelay(t *testing.T) {
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
				config:     dockerWatcherConfig{autoScaleDown: true},
				serviceMap: map[string]*routableSwarmService{},
			}

			for _, step := range []struct {
				name    string
				value   string
				present bool
				want    *time.Duration
			}{
				{name: "inherit"},
				{
					name:    "add",
					value:   "12h",
					present: true,
					want:    new(12 * time.Hour),
				},
				{
					name:    "timeout-only update",
					value:   "5m",
					present: true,
					want:    new(5 * time.Minute),
				},
				{name: "remove"},
				{
					name:    "trim",
					value:   " 30s ",
					present: true,
					want:    new(30 * time.Second),
				},
				{
					name:    "invalid clears override",
					value:   "forever",
					present: true,
				},
				{
					name:    "restore",
					value:   "1h",
					present: true,
					want:    new(time.Hour),
				},
				{
					name:    "zero",
					value:   "0s",
					present: true,
				},
				{
					name:    "negative",
					value:   "-1h",
					present: true,
				},
				{
					name:    "empty",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					delete(labels, DockerRouterLabelAutoScaleDownAfter)
					if step.present {
						labels[DockerRouterLabelAutoScaleDownAfter] = step.value
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
					_, _, target, _, sleeper, _ := routes.FindBackendForServerAddress(t.Context(), host)
					require.NotNil(t, sleeper)
					want := 10 * time.Minute
					if step.want != nil {
						want = *step.want
					}
					assert.Equal(t, want, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

				})
			}
		})
	}
}

func TestKubernetesDiscoveryScaleDownDelay(t *testing.T) {
	// The sequence exercises creation, override-only updates, removal, and invalid
	// input after a valid override. All other discovery metadata stays unchanged.
	for _, defaultRoute := range []bool{false, true} {
		name := "named"
		if defaultRoute {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			watcher := &K8sWatcher{routesHandler: routes, autoScaleDown: true}
			var previous *core.Service

			for _, step := range []struct {
				name    string
				value   string
				present bool
				want    *time.Duration
			}{
				{name: "inherit"},
				{
					name:    "add",
					value:   "12h",
					present: true,
					want:    new(12 * time.Hour),
				},
				{
					name:    "timeout-only update",
					value:   "5m",
					present: true,
					want:    new(5 * time.Minute),
				},
				{name: "remove"},
				{
					name:    "trim",
					value:   " 30s ",
					present: true,
					want:    new(30 * time.Second),
				},
				{
					name:    "invalid clears override",
					value:   "forever",
					present: true,
				},
				{
					name:    "restore",
					value:   "1h",
					present: true,
					want:    new(time.Hour),
				},
				{
					name:    "zero",
					value:   "0s",
					present: true,
				},
				{
					name:    "negative",
					value:   "-1h",
					present: true,
				},
				{
					name:    "empty",
					present: true,
				},
			} {
				t.Run(step.name, func(t *testing.T) {
					annotations := map[string]string{AnnotationExternalServerName: "mc.example.com"}
					if defaultRoute {
						annotations = map[string]string{AnnotationDefaultServer: "true"}
					}
					if step.present {
						annotations[AnnotationAutoScaleDownAfter] = step.value
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
					_, _, target, _, sleeper, _ := routes.FindBackendForServerAddress(t.Context(), host)
					require.NotNil(t, sleeper)
					want := 10 * time.Minute
					if step.want != nil {
						want = *step.want
					}
					assert.Equal(t, want, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

				})
			}
		})
	}
}
