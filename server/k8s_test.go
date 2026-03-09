package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

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

func (m *MockedRoutesHandler) CreateMapping(serverAddress string, backend string, scaleKey string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	m.MethodCalled("CreateMapping", serverAddress, backend, scaleKey, waker, sleeper, asleepMOTD, loadingMOTD)
	if m.routes == nil {
		m.routes = make(map[string]string)
	}
	m.routes[serverAddress] = backend
}

func (m *MockedRoutesHandler) SetDefaultRoute(backend string, scaleKey string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	m.MethodCalled("SetDefaultRoute", backend, scaleKey, waker, sleeper, asleepMOTD, loadingMOTD)
	if m.routes == nil {
		m.routes = make(map[string]string)
	}
	m.defaultBackend = backend
}

func (m *MockedRoutesHandler) GetAsleepMOTD(serverAddress string) string {
	args := m.MethodCalled("GetAsleepMOTD", serverAddress)
	return args.String(0)
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
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
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
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
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
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
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

func TestK8s_proxyServerName(t *testing.T) {
	type scenario struct {
		server  string
		backend string
	}
	tests := []struct {
		name      string
		svc       string
		scenarios []scenario
	}{
		{
			name: "proxy routes to proxy address",
			svc:  `{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com", "mc-router.itzg.me/proxyServerName": "velocity-proxy:25577"}}, "spec":{"clusterIP": "10.0.0.5"}}`,
			scenarios: []scenario{
				{server: "mc.example.com", backend: "velocity-proxy:25577"},
			},
		},
		{
			name: "proxy without port gets default 25565",
			svc:  `{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com", "mc-router.itzg.me/proxyServerName": "velocity-proxy"}}, "spec":{"clusterIP": "10.0.0.5"}}`,
			scenarios: []scenario{
				{server: "mc.example.com", backend: "velocity-proxy:25565"},
			},
		},
		{
			name: "no proxy annotation routes to ClusterIP",
			svc:  `{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com"}}, "spec":{"clusterIP": "10.0.0.5"}}`,
			scenarios: []scenario{
				{server: "mc.example.com", backend: "10.0.0.5:25565"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

			routesHandler := new(MockedRoutesHandler)
			routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
			routesHandler.On("DeleteMapping", mock.Anything).Return(true)

			watcher := &K8sWatcher{
				routesHandler: routesHandler,
			}
			svc := v1.Service{}
			err := json.Unmarshal([]byte(test.svc), &svc)
			require.NoError(t, err)

			watcher.handleAdd(&svc)
			for _, s := range test.scenarios {
				backend := routesHandler.GetBackendForServer(s.server)
				assert.Equal(t, s.backend, backend, "given=%s", s.server)
			}
		})
	}
}

func TestK8s_proxyServerNameScaleEndpoint(t *testing.T) {
	DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

	routesHandler := new(MockedRoutesHandler)
	routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
	routesHandler.On("DeleteMapping", mock.Anything).Return(true)

	watcher := &K8sWatcher{
		routesHandler: routesHandler,
	}

	svc := v1.Service{}
	err := json.Unmarshal([]byte(`{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com", "mc-router.itzg.me/proxyServerName": "velocity:25577"}}, "spec":{"clusterIP": "10.0.0.5"}}`), &svc)
	require.NoError(t, err)

	watcher.handleAdd(&svc)

	// Verify CreateMapping was called with the correct scaleKey (original endpoint)
	routesHandler.AssertCalled(t, "CreateMapping", "mc.example.com", "velocity:25577", "10.0.0.5:25565", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestK8s_proxyServerNameUpdate(t *testing.T) {
	DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

	routesHandler := new(MockedRoutesHandler)
	routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
	routesHandler.On("DeleteMapping", mock.Anything).Return(true)

	watcher := &K8sWatcher{
		routesHandler: routesHandler,
	}

	// Start with proxy
	initialSvc := v1.Service{}
	err := json.Unmarshal([]byte(`{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com", "mc-router.itzg.me/proxyServerName": "velocity:25577"}}, "spec":{"clusterIP": "10.0.0.5"}}`), &initialSvc)
	require.NoError(t, err)

	watcher.handleAdd(&initialSvc)
	assert.Equal(t, "velocity:25577", routesHandler.GetBackendForServer("mc.example.com"))

	// Update to remove proxy
	updatedSvc := v1.Service{}
	err = json.Unmarshal([]byte(`{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com"}}, "spec":{"clusterIP": "10.0.0.5"}}`), &updatedSvc)
	require.NoError(t, err)

	watcher.handleUpdate(&initialSvc, &updatedSvc)
	assert.Equal(t, "10.0.0.5:25565", routesHandler.GetBackendForServer("mc.example.com"))
}

func TestK8s_autoScaleWithoutProxy(t *testing.T) {
	DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

	routesHandler := new(MockedRoutesHandler)
	routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
	routesHandler.On("DeleteMapping", mock.Anything).Return(true)

	watcher := &K8sWatcher{
		autoScaleUp:   true,
		autoScaleDown: true,
		routesHandler: routesHandler,
	}

	// Service WITHOUT proxyServerName but WITH autoScaleUp/Down annotations
	svc := v1.Service{}
	err := json.Unmarshal([]byte(`{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "atm-10.example.com", "mc-router.itzg.me/autoScaleUp": "true", "mc-router.itzg.me/autoScaleDown": "true"}}, "spec":{"clusterIP": "10.0.0.10"}}`), &svc)
	require.NoError(t, err)

	watcher.handleAdd(&svc)

	// Verify routes to ClusterIP (not proxy)
	assert.Equal(t, "10.0.0.10:25565", routesHandler.GetBackendForServer("atm-10.example.com"))

	// CRITICAL: Verify scaleKey is set to the service endpoint (not empty)
	// This ensures auto-scaling targets the correct StatefulSet
	routesHandler.AssertCalled(t, "CreateMapping", "atm-10.example.com", "10.0.0.10:25565", "10.0.0.10:25565", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestBuildK8sWaker_NilScaleUp(t *testing.T) {
	waker := buildK8sWaker("10.0.0.1:25565", nil, 60*time.Second)
	assert.Nil(t, waker, "buildK8sWaker should return nil when scaleUp is nil")
}

func TestBuildK8sWaker_WaitsForEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	endpoint := ln.Addr().String()

	scaleUpCalled := false
	scaleUp := func(ctx context.Context) error {
		scaleUpCalled = true
		return nil
	}

	waker := buildK8sWaker(endpoint, scaleUp, 60*time.Second)
	require.NotNil(t, waker)

	result, err := waker(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, endpoint, result)
	assert.True(t, scaleUpCalled, "scaleUp should have been called")
}

func TestBuildK8sWaker_ScaleUpError(t *testing.T) {
	scaleUp := func(ctx context.Context) error {
		return fmt.Errorf("scale up failed")
	}

	waker := buildK8sWaker("10.0.0.1:25565", scaleUp, 60*time.Second)
	require.NotNil(t, waker)

	_, err := waker(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scale up failed")
}

func TestBuildK8sWaker_ContextCancellation(t *testing.T) {
	scaleUp := func(ctx context.Context) error {
		return nil
	}

	waker := buildK8sWaker("192.0.2.1:65534", scaleUp, 60*time.Second)
	require.NotNil(t, waker)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := waker(ctx)
	assert.Error(t, err, "waker should return error when context is cancelled")
}

func TestK8s_motdAnnotations(t *testing.T) {
	DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

	routesHandler := new(MockedRoutesHandler)
	routesHandler.On("CreateMapping", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("SetDefaultRoute", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	routesHandler.On("GetAsleepMOTD", mock.Anything).Return("")
	routesHandler.On("DeleteMapping", mock.Anything).Return(true)

	watcher := &K8sWatcher{
		autoScaleUp:   true,
		routesHandler: routesHandler,
	}

	svc := v1.Service{}
	err := json.Unmarshal([]byte(`{"metadata": {"annotations": {"mc-router.itzg.me/externalServerName": "mc.example.com", "mc-router.itzg.me/autoScaleUp": "true", "mc-router.itzg.me/autoScaleAsleepMOTD": "Server is sleeping", "mc-router.itzg.me/autoScaleLoadingMOTD": "Server is starting"}}, "spec":{"clusterIP": "10.0.0.5"}}`), &svc)
	require.NoError(t, err)

	watcher.handleAdd(&svc)

	routesHandler.AssertCalled(t, "CreateMapping", "mc.example.com", "10.0.0.5:25565", "10.0.0.5:25565", mock.Anything, mock.Anything, "Server is sleeping", "Server is starting")
}
