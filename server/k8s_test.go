package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

func TestK8sWatcherImpl_handleAddThenUpdate(t *testing.T) {
	type scenario struct {
		given  string
		expect string
	}
	type svcAndScenarios struct {
		svc       string
		scenarios []scenario
	}
	tests := []struct {
		name    string
		initial svcAndScenarios
		update  svcAndScenarios
	}{
		{
			name: "a to b",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: ""},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "a to a,b",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "a,b to b",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: ""},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)
			Routes.Reset()

			watcher := &K8sWatcher{}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "initial: given=%s", s.given)
			}

			updatedSvc := v1.Service{}
			err = json.Unmarshal([]byte(test.update.svc), &updatedSvc)
			require.NoError(t, err)

			watcher.handleUpdate(&initialSvc, &updatedSvc)
			for _, s := range test.update.scenarios {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "update: given=%s", s.given)
			}
		})
	}
}

func TestK8sWatcherImpl_handleAddThenDelete(t *testing.T) {
	type scenario struct {
		given  string
		expect string
	}
	type svcAndScenarios struct {
		svc       string
		scenarios []scenario
	}
	tests := []struct {
		name    string
		initial svcAndScenarios
		delete  []scenario
	}{
		{
			name: "single",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: ""},
				},
			},
			delete: []scenario{
				{given: "a.com", expect: ""},
				{given: "b.com", expect: ""},
			},
		},
		{
			name: "multi",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
			delete: []scenario{
				{given: "a.com", expect: ""},
				{given: "b.com", expect: ""},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)
			Routes.Reset()

			watcher := &K8sWatcher{}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "initial: given=%s", s.given)
			}

			watcher.handleDelete(&initialSvc)
			for _, s := range test.delete {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "update: given=%s", s.given)
			}
		})
	}
}

func TestK8s_externalName(t *testing.T) {
	type scenario struct {
		given  string
		expect string
	}
	type svcAndScenarios struct {
		svc       string
		scenarios []scenario
	}
	tests := []struct {
		name    string
		initial svcAndScenarios
		update  svcAndScenarios
	}{
		{
			name: "typeChange",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"type":"ExternalName", "externalName": "mc-server.com"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "mc-server.com:25565"},
					{given: "b.com", expect: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: ""},
				},
			},
		},
		{
			name: "typeAndServerChange",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"type":"ExternalName", "externalName": "mc-server.com"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "mc-server.com:25565"},
					{given: "b.com", expect: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: ""},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "externalNameChange",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"type":"ExternalName", "externalName": "mc-server.com"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "mc-server.com:25565"},
					{given: "b.com", expect: "mc-server.com:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"type":"ExternalName", "externalName": "1.1.1.1"}}`,
				scenarios: []scenario{
					{given: "a.com", expect: "1.1.1.1:25565"},
					{given: "b.com", expect: "1.1.1.1:25565"},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)
			Routes.Reset()

			watcher := &K8sWatcher{}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "initial: given=%s", s.given)
			}

			updatedSvc := v1.Service{}
			err = json.Unmarshal([]byte(test.update.svc), &updatedSvc)
			require.NoError(t, err)

			watcher.handleUpdate(&initialSvc, &updatedSvc)
			for _, s := range test.update.scenarios {
				backend, _, _, _ := Routes.FindBackendForServerAddress(context.Background(), s.given)
				assert.Equal(t, s.expect, backend, "update: given=%s", s.given)
			}
		})
	}
}
