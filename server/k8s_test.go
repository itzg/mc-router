package server

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/mock"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

type MockedRoutesHandler struct {
	mock.Mock
	routes         map[string]string
	defaultBackend string
}

func (m *MockedRoutesHandler) GetBackendForServer(server string) string {
	backend, exists := m.routes[server]
	if exists {
		return backend
	} else {
		return m.defaultBackend
	}
}

func (m *MockedRoutesHandler) CreateMapping(serverAddress string, backend string, waker ScalerFunc, sleeper ScalerFunc) {
	m.MethodCalled("CreateMapping", serverAddress, backend, waker, sleeper)
	if m.routes == nil {
		m.routes = make(map[string]string)
	}
	m.routes[serverAddress] = backend
}

func (m *MockedRoutesHandler) SetDefaultRoute(backend string) {
	m.MethodCalled("SetDefaultRoute", backend)
	if m.routes == nil {
		m.routes = make(map[string]string)
	}
	m.defaultBackend = backend
}

func (m *MockedRoutesHandler) DeleteMapping(serverAddress string) bool {
	args := m.MethodCalled("DeleteMapping", serverAddress)
	if m.routes == nil {
		m.routes = make(map[string]string)
	}
	delete(m.routes, serverAddress)
	return args.Bool(0)
}

func TestK8sWatcherImpl_handleAddThenUpdate(t *testing.T) {
	type scenario struct {
		server  string
		backend string
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
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "a to a,b",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "a,b to b",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "comma with spaces",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com, b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "newline separated",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com\nb.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "mixed comma and newline with spaces",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com, \nb.com,  c.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
					{server: "c.com", backend: "1.1.1.1:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
					{server: "c.com", backend: ""},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

			routesHandler := new(MockedRoutesHandler)
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("DeleteMapping", mock.Anything).Return(true)

			watcher := &K8sWatcher{
				routesHandler: routesHandler,
			}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "initial: given=%s", s.server)
			}

			updatedSvc := v1.Service{}
			err = json.Unmarshal([]byte(test.update.svc), &updatedSvc)
			require.NoError(t, err)

			watcher.handleUpdate(&initialSvc, &updatedSvc)
			for _, s := range test.update.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "update: given=%s", s.server)
			}
		})
	}
}

func TestK8sWatcherImpl_handleAddThenDelete(t *testing.T) {
	type scenario struct {
		server  string
		backend string
	}
	type svcAndScenarios struct {
		svc       string
		scenarios []scenario
	}
	tests := []struct {
		name    string
		initial svcAndScenarios
		// non-empty `backend` in this case means the server is expected to be deleted
		delete []scenario
	}{
		{
			name: "single",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: ""},
				},
			},
			delete: []scenario{
				{server: "a.com", backend: ""},
				{server: "b.com", backend: ""},
			},
		},
		{
			name: "multi",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
			delete: []scenario{
				{server: "a.com", backend: ""},
				{server: "b.com", backend: ""},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

			routesHandler := new(MockedRoutesHandler)
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("DeleteMapping", mock.Anything).Return(true)

			watcher := &K8sWatcher{
				routesHandler: routesHandler,
			}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "initial: given=%s", s.server)
			}

			watcher.handleDelete(&initialSvc)
			for _, s := range test.delete {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "update: given=%s", s.server)
			}
		})
	}
}

func TestK8s_externalName(t *testing.T) {
	type scenario struct {
		server  string
		backend string
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
					{server: "a.com", backend: "mc-server.com:25565"},
					{server: "b.com", backend: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: ""},
				},
			},
		},
		{
			name: "typeAndServerChange",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com"}}, "spec":{"type":"ExternalName", "externalName": "mc-server.com"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "mc-server.com:25565"},
					{server: "b.com", backend: ""},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "b.com"}}, "spec":{"clusterIP": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: ""},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
		{
			name: "externalNameChange",
			initial: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"type":"ExternalName", "externalName": "mc-server.com"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "mc-server.com:25565"},
					{server: "b.com", backend: "mc-server.com:25565"},
				},
			},
			update: svcAndScenarios{
				svc: ` {"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "a.com,b.com"}}, "spec":{"type":"ExternalName", "externalName": "1.1.1.1"}}`,
				scenarios: []scenario{
					{server: "a.com", backend: "1.1.1.1:25565"},
					{server: "b.com", backend: "1.1.1.1:25565"},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// DownScaler needs to be instantiated
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

			routesHandler := new(MockedRoutesHandler)
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("DeleteMapping", mock.Anything).Return(true)

			watcher := &K8sWatcher{
				routesHandler: routesHandler,
			}
			initialSvc := v1.Service{}
			err := json.Unmarshal([]byte(test.initial.svc), &initialSvc)
			require.NoError(t, err)

			watcher.handleAdd(&initialSvc)
			for _, s := range test.initial.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "initial: given=%s", s.server)
			}

			updatedSvc := v1.Service{}
			err = json.Unmarshal([]byte(test.update.svc), &updatedSvc)
			require.NoError(t, err)

			watcher.handleUpdate(&initialSvc, &updatedSvc)
			for _, s := range test.update.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "update: given=%s", s.server)
			}
		})
	}
}
