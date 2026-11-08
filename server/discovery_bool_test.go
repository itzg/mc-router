package server

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDockerInvalidBooleanLabelsRejectContainer(t *testing.T) {
	for _, label := range []string{DockerRouterLabelDefault, DockerRouterLabelAutoScaleUp, DockerRouterLabelAutoScaleDown} {
		t.Run(label, func(t *testing.T) {
			inspect := &container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "minecraft",
					Name:  "minecraft",
					State: &container.State{Running: true},
				},
				Config: &container.Config{Labels: map[string]string{
					DockerRouterLabelHost: "mc.example.com",
					label:                 "yes",
				}},
				NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
					"minecraft": {IPAddress: "10.0.0.1"},
				}},
			}
			watcher := &dockerWatcherImpl{}
			_, ok := watcher.parseContainerData(inspect)
			assert.False(t, ok, "invalid autoscale or default labels must still reject the container")
		})
	}
}

func TestSwarmInvalidBooleanLabelsRejectService(t *testing.T) {
	for _, label := range []string{DockerRouterLabelAutoScaleUp, DockerRouterLabelAutoScaleDown} {
		t.Run(label, func(t *testing.T) {
			service := &swarm.Service{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Labels: map[string]string{
				DockerRouterLabelHost: "mc.example.com",
				label:                 "yes",
			}}}}
			watcher := &dockerSwarmWatcherImpl{}
			assert.False(t, watcher.parseServiceLabels(service, &parsedDockerServiceData{}))
		})
	}
}

func TestSwarmDefaultLabelRemainsPermissive(t *testing.T) {
	// Swarm default labels historically accept truthy values such as yes. They
	// must not adopt the strict parsing used for autoscale and PROXY settings.
	for _, test := range []struct {
		value string
		want  bool
	}{
		{
			value: "yes",
			want:  true,
		},
		{
			value: "anything",
			want:  true,
		},
		{
			value: " NO ",
		},
		{
			value: " False ",
		},
		{
			value: "0",
		},
		{
			value: "",
		},
	} {
		t.Run(test.value, func(t *testing.T) {
			service := &swarm.Service{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Labels: map[string]string{
				DockerRouterLabelDefault: test.value,
			}}}}
			watcher := &dockerSwarmWatcherImpl{}
			var data parsedDockerServiceData
			require.True(t, watcher.parseServiceLabels(service, &data))
			require.NotNil(t, data.def)
			assert.Equal(t, test.want, *data.def)
		})
	}
}

func TestKubernetesInvalidBooleanAnnotationsDisableScaling(t *testing.T) {
	for _, test := range []struct {
		annotation string
		from       int32
		to         int32
	}{
		{
			annotation: AnnotationAutoScaleUp,
			from:       0,
			to:         1,
		},
		{
			annotation: AnnotationAutoScaleDown,
			from:       1,
			to:         0,
		},
	} {
		t.Run(test.annotation, func(t *testing.T) {
			watcher := &K8sWatcher{autoScaleUp: true, autoScaleDown: true}
			service := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "minecraft"}}
			require.NotNil(t, watcher.buildScaleFunction(service, test.from, test.to), "absent annotations inherit enabled global scaling")
			service.Annotations = map[string]string{test.annotation: "yes"}
			assert.Nil(t, watcher.buildScaleFunction(service, test.from, test.to), "invalid annotations must disable the affected scaling callback")
		})
	}
}
