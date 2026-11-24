package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKubernetesResourcePolicyDiscoveryOrder(t *testing.T) {
	tests := []struct {
		name     string
		setFirst bool
		active   bool
		proxy    bool
	}{
		{name: "service first"},
		{name: "proxy routing endpoint", proxy: true},
		{name: "statefulset first", setFirst: true},
		{name: "active during discovery", active: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := NewDownScaler(true, time.Hour).(*downScalerImpl)
			t.Cleanup(ds.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(ds)
			watcher := &K8sWatcher{routesHandler: routes, autoScaleDown: true}
			service := &core.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "minecraft",
					Namespace: "games",
					Annotations: map[string]string{
						AnnotationExternalServerName: "first.example.com,second.example.com",
						AnnotationAutoScaleDownAfter: "24h",
					}},
				Spec: core.ServiceSpec{ClusterIP: "10.0.0.1"},
			}
			if tt.proxy {
				service.Annotations[AnnotationProxyServerName] = "proxy:25565"
			}
			set := statefulSetIn("games", "different-set-name", "minecraft")
			set.Annotations = map[string]string{AnnotationAutoScaleDownAfter: "2h"}
			if tt.setFirst {
				watcher.handleAddStatefulSet()(set)
			}
			watcher.handleAdd(service)
			_, _, target, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "first.example.com")
			require.NotNil(t, target)
			if !tt.setFirst {
				assert.True(t, target.ScaleDownPolicy().pending)
				assert.Empty(t, ds.timers)
			}
			if tt.active {
				ds.ConnectionOpened(target)
			}
			if !tt.setFirst {
				watcher.handleAddStatefulSet()(set)
			}
			_, _, alias, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "second.example.com")
			require.Same(t, target, alias)
			assert.Equal(t, 2*time.Hour, target.ScaleDownPolicy().resolve(time.Minute))
			original := ds.timers[target.ScalingKey()]
			if tt.active {
				require.Nil(t, original)
			} else {
				require.NotNil(t, original)
			}

			updated := set.DeepCopy()
			updated.Annotations[AnnotationAutoScaleDownAfter] = "3h"
			watcher.handleUpdateStatefulSet()(set, updated)
			assert.Same(t, original, ds.timers[target.ScalingKey()])
			assert.Equal(t, 3*time.Hour, alias.ScaleDownPolicy().resolve(time.Minute))
			watcher.handleDeleteStatefulSet()(updated)
			assert.True(t, target.ScaleDownPolicy().pending)
			assert.Empty(t, ds.timers)
		})
	}
}

func TestKubernetesResourcePolicyStatusUpdate(t *testing.T) {
	handler := setupRoutesHandlerMock()
	watcher := &K8sWatcher{routesHandler: handler, autoScaleDown: true}
	service := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "minecraft",
			Namespace: "games",
			Annotations: map[string]string{
				AnnotationExternalServerName: "mc.example.com",
			}},
		Spec: core.ServiceSpec{ClusterIP: "10.0.0.1"},
	}
	watcher.handleAdd(service)
	set := statefulSetIn("games", "server", "minecraft")
	watcher.handleAddStatefulSet()(set)
	callsBeforeStatus := len(handler.Calls)
	updated := set.DeepCopy()
	updated.Status.ReadyReplicas = 1
	watcher.handleUpdateStatefulSet()(set, updated)
	assert.Len(t, handler.Calls, callsBeforeStatus, "status-only events must not republish routes")

	// Empty is invalid metadata, distinct from an omitted annotation. It must be
	// processed even though both resolve to the global fallback.
	updated.Annotations = map[string]string{AnnotationAutoScaleDownAfter: ""}
	watcher.handleUpdateStatefulSet()(set, updated)
	assert.Greater(t, len(handler.Calls), callsBeforeStatus)
}

func TestKubernetesResourcePolicyReassociation(t *testing.T) {
	routes := NewRoutes(t.Context())
	watcher := &K8sWatcher{routesHandler: routes, autoScaleDown: true}
	for _, name := range []string{"first", "second"} {
		watcher.handleAdd(&core.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "games",
				Annotations: map[string]string{
					AnnotationExternalServerName: name + ".example.com",
				}},
			Spec: core.ServiceSpec{ClusterIP: "10.0.0.1"},
		})
	}
	set := statefulSetIn("games", "server", "first")
	set.Annotations = map[string]string{AnnotationAutoScaleDownAfter: "2h"}
	watcher.handleAddStatefulSet()(set)
	_, _, first, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "first.example.com")
	_, _, second, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "second.example.com")
	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.False(t, first.ScaleDownPolicy().pending)
	assert.True(t, second.ScaleDownPolicy().pending)

	updated := set.DeepCopy()
	updated.Spec.ServiceName = "second"
	watcher.handleUpdateStatefulSet()(set, updated)
	assert.True(t, first.ScaleDownPolicy().pending)
	assert.Equal(t, ScaleDownPolicy{delay: 2 * time.Hour}, second.ScaleDownPolicy())
}
