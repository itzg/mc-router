package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_routesImpl_FindBackendForServerAddress(t *testing.T) {
	type args struct {
		serverAddress string
	}
	type mapping struct {
		serverAddress string
		backend       string
	}
	tests := []struct {
		name    string
		mapping mapping
		args    args
		want    string
	}{
		{
			name: "typical",
			mapping: mapping{
				serverAddress: "typical.my.domain", backend: "backend:25565",
			},
			args: args{
				serverAddress: `typical.my.domain`,
			},
			want: "backend:25565",
		},
		{
			name: "forge",
			mapping: mapping{
				serverAddress: "forge.my.domain", backend: "backend:25566",
			},
			args: args{
				serverAddress: "forge.my.domain\x00FML2\x00",
			},
			want: "backend:25566",
		},
		{
			name: "root zone indicator",
			mapping: mapping{
				serverAddress: "my.domain", backend: "backend:25566",
			},
			args: args{
				serverAddress: "my.domain.",
			},
			want: "backend:25566",
		},
		{
			name: "root zone indicator and forge",
			mapping: mapping{
				serverAddress: "forge.my.domain", backend: "backend:25566",
			},
			args: args{
				serverAddress: "forge.my.domain.\x00FML2\x00",
			},
			want: "backend:25566",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRoutes(t.Context())

			r.CreateMapping(tt.mapping.serverAddress, tt.mapping.backend, nil, nil, nil, "", "")

			if got, server, _, _, _ := r.FindBackendForServerAddress(context.Background(), tt.args.serverAddress); got != tt.want {
				t.Errorf("routesImpl.FindBackendForServerAddress() = %v, want %v", got, tt.want)
			} else {
				assert.Equal(t, tt.mapping.serverAddress, server)
			}
		})
	}
}

type testingScalingTarget struct {
	ScalingIndicator
	name string
}

func TestingScalingTarget(name string) ScalingTarget {
	return &testingScalingTarget{name: name, ScalingIndicator: ScalingIndicator{scaling: &atomic.Bool{}}}
}

func (n *testingScalingTarget) String() string {
	if n == nil {
		return ""
	}
	return n.name
}

func (n *testingScalingTarget) ScalingKey() string {
	return n.name
}

func Test_routesImpl_ScaleKey(t *testing.T) {
	downScaler := NewDownScaler(false, 1*time.Second)

	addressProxy := "proxy:25577"
	targetProxy := TestingScalingTarget(addressProxy)
	target5 := TestingScalingTarget("10.0.0.5:25565")
	t.Run("scaleKey is set when provided", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.CreateMapping("mc.example.com", addressProxy, target5, nil, nil, "", "")

		backend, _, scalingTarget, _, _ := r.FindBackendForServerAddress(context.Background(), "mc.example.com")
		assert.Equal(t, addressProxy, backend)
		assert.Equal(t, "10.0.0.5:25565", scalingTarget.ScalingKey())
	})

	t.Run("GetSleepers matches on scaleKey not backend", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		called := false
		sleeper := func(ctx context.Context) error {
			called = true
			return nil
		}

		// Two routes with same proxy backend but different scaleKeys
		target1 := TestingScalingTarget("10.0.0.1:25565")
		r.CreateMapping("mc1.example.com", addressProxy, target1, nil, sleeper, "", "")
		target2 := TestingScalingTarget("10.0.0.2:25565")
		r.CreateMapping("mc2.example.com", addressProxy, target2, nil, nil, "", "")

		s := r.GetSleeper(target1)
		require.NotNil(t, s)
		// call the sleeper to ensure it's the one registered
		_ = s(context.Background())
		assert.True(t, called)

		// No sleeper for the second scaleKey since it has nil sleeper
		s = r.GetSleeper(target2)
		assert.Nil(t, s)

		// No sleeper when querying by proxy backend address
		s = r.GetSleeper(targetProxy)
		assert.Nil(t, s)
	})

	t.Run("default route scaleKey", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.SetDefaultRoute(addressProxy, target5, nil, nil, "", "")

		backend, scalingTarget, _, _ := r.GetDefaultRoute()
		assert.Equal(t, addressProxy, backend)
		assert.Equal(t, target5.ScalingKey(), scalingTarget.ScalingKey())
	})

	t.Run("default route scaleKey defaults to backend", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.SetDefaultRoute("backend:25565", nil, nil, nil, "", "")

		backend, scalingTarget, _, _ := r.GetDefaultRoute()
		assert.Equal(t, "backend:25565", backend)
		assert.Nil(t, scalingTarget)
	})
}

func Test_routesImpl_LoadingMOTD(t *testing.T) {
	r := NewRoutes(t.Context())
	r.CreateMapping("mc.example.com", "backend:25565", nil, nil, nil, "asleep", "loading")

	assert.Equal(t, "loading", r.GetLoadingMOTD("mc.example.com"))
	assert.Equal(t, "", r.GetLoadingMOTD("other.example.com"))

	r.SetDefaultRoute("default:25565", nil, nil, nil, "default asleep", "default loading")
	assert.Equal(t, "default loading", r.GetLoadingMOTD(""))
}

type mockRoutesListener struct {
	mock.Mock
}

func (m *mockRoutesListener) OnRouteAdded(serverAddress string, backend string) {
	m.Called(serverAddress, backend)
}

func (m *mockRoutesListener) OnDefaultRouteSet(backend string) {
	m.Called(backend)
}

func (m *mockRoutesListener) OnRouteRemoved(serverAddress string) {
	m.Called(serverAddress)
}

func (m *mockRoutesListener) OnDefaultRouteRemoved() {
	m.Called()
}

func TestRoutesListener_OnRouteAdded(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)

	r.CreateMapping("mc.example.com", "backend:25565", nil, nil, nil, "asleep", "loading")

	listener.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")
}

func TestRoutesListener_OnRouteRemoved(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	listener.On("OnRouteRemoved", "mc.example.com").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.CreateMapping("mc.example.com", "backend:25565", nil, nil, nil, "asleep", "loading")
	listener.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")

	r.RemoveMapping("mc.example.com")
	listener.AssertCalled(t, "OnRouteRemoved", "mc.example.com")
}

func TestRoutesListener_OnRouteRemoved_withoutDownScaler(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	listener.On("OnRouteRemoved", "mc.example.com").Return()

	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(nil)

	r.CreateMapping("mc.example.com", "backend:25565", TestingScalingTarget("mc.example.com"), nil, nil, "", "")

	assert.True(t, r.RemoveMapping("mc.example.com"))
	listener.AssertCalled(t, "OnRouteRemoved", "mc.example.com")
	assert.False(t, r.HasRoute("mc.example.com"))
}

func TestRoutesListener_OnDefaultRouteAdded(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnDefaultRouteSet", "default:25565").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)

	r.SetDefaultRoute("default:25565", nil, nil, nil, "", "")

	listener.AssertCalled(t, "OnDefaultRouteSet", "default:25565")
}

func TestRoutesListener_OnDefaultRouteRemoved_dueToReset(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnDefaultRouteSet", "default:25565").Return()
	listener.On("OnDefaultRouteRemoved").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.SetDefaultRoute("default:25565", nil, nil, nil, "", "")
	listener.AssertCalled(t, "OnDefaultRouteSet", "default:25565")

	r.Reset()
	listener.AssertCalled(t, "OnDefaultRouteRemoved")
}

func TestRoutesListener_OnDefaultRouteRemoved_dueToDeleteDefaultRoute(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnDefaultRouteSet", "default:25565").Return()
	listener.On("OnDefaultRouteRemoved").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)

	r.SetDefaultRoute("default:25565", nil, nil, nil, "", "")
	listener.AssertCalled(t, "OnDefaultRouteSet", "default:25565")

	r.RemoveDefaultRoute()
	listener.AssertCalled(t, "OnDefaultRouteRemoved")
}

func TestRoutesListener_MultipleListeners(t *testing.T) {
	listener1 := &mockRoutesListener{}
	listener2 := &mockRoutesListener{}
	listener1.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	listener2.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener1).
		WithListener(listener2)

	r.CreateMapping("mc.example.com", "backend:25565", nil, nil, nil, "asleep", "loading")

	listener1.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")
	listener2.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")
}

func TestRoutesListener_ResetCallsOnRouteRemovedForAllRoutes(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteAdded", mock.Anything, mock.Anything).Return()
	listener.On("OnRouteRemoved", mock.Anything).Return()
	listener.On("OnDefaultRouteRemoved").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.CreateMapping("mc1.example.com", "backend:25565", nil, nil, nil, "", "")
	r.CreateMapping("mc2.example.com", "backend:25566", nil, nil, nil, "", "")
	r.CreateMapping("mc3.example.com", "backend:25567", nil, nil, nil, "", "")

	listener.AssertCalled(t, "OnRouteAdded", "mc1.example.com", "backend:25565")
	listener.AssertCalled(t, "OnRouteAdded", "mc2.example.com", "backend:25566")
	listener.AssertCalled(t, "OnRouteAdded", "mc3.example.com", "backend:25567")

	r.Reset()

	listener.AssertCalled(t, "OnRouteRemoved", "mc1.example.com")
	listener.AssertCalled(t, "OnRouteRemoved", "mc2.example.com")
	listener.AssertCalled(t, "OnRouteRemoved", "mc3.example.com")
}

func TestRoutesListener_DeleteNonExistentRouteDoesNotNotifyListener(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteRemoved", mock.Anything).Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	deleted := r.RemoveMapping("nonexistent.example.com")
	assert.False(t, deleted)

	listener.AssertNotCalled(t, "OnRouteRemoved", "nonexistent.example.com")
}

func TestRoutesListener_NilListenersHandled(t *testing.T) {
	r := NewRoutes(t.Context())
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.CreateMapping("mc.example.com", "backend:25565", nil, nil, nil, "", "")
	r.SetDefaultRoute("default:25565", nil, nil, nil, "", "")
	r.RemoveMapping("mc.example.com")
	r.Reset()
}

type MockRoutes struct {
	mock.Mock
}

func (m *MockRoutes) CreateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	m.MethodCalled("CreateMapping", serverAddress, backend, scalingTarget, waker, sleeper, asleepMOTD, loadingMOTD)
}

func (m *MockRoutes) UpdateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	m.MethodCalled("UpdateMapping", serverAddress, backend, scalingTarget, waker, sleeper, asleepMOTD, loadingMOTD)
}

func (m *MockRoutes) SetDefaultRoute(backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	m.MethodCalled("SetDefaultRoute", backend, scalingTarget, waker, sleeper, asleepMOTD, loadingMOTD)
}

func (m *MockRoutes) RemoveDefaultRoute() {
	m.MethodCalled("RemoveDefaultRoute")
}

func (m *MockRoutes) RemoveMapping(serverAddress string) bool {
	args := m.MethodCalled("RemoveMapping", serverAddress)
	return args.Bool(0)
}

func (m *MockRoutes) Reset() {
	m.MethodCalled("Reset")
}

func nillableScalingTarget(in interface{}) ScalingTarget {
	var scalingTarget ScalingTarget
	if in != nil {
		scalingTarget = in.(ScalingTarget)
	}
	return scalingTarget
}

func nillableWaker(in interface{}) WakerFunc {
	var waker WakerFunc
	if in != nil {
		waker = in.(WakerFunc)
	}
	return waker
}

func nillableSleeper(in interface{}) SleeperFunc {
	var sleeper SleeperFunc
	if in != nil {
		sleeper = in.(SleeperFunc)
	}
	return sleeper
}

func (m *MockRoutes) FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, ScalingTarget, WakerFunc, SleeperFunc) {
	args := m.MethodCalled("FindBackendForServerAddress", ctx, serverAddress)

	return args.String(0), args.String(1), nillableScalingTarget(args.Get(2)), nillableWaker(args.Get(3)), nillableSleeper(args.Get(4))
}

func (m *MockRoutes) HasRoute(serverAddress string) bool {
	args := m.MethodCalled("HasRoute", serverAddress)
	return args.Bool(0)
}

func (m *MockRoutes) GetSleeper(scalingTarget ScalingTarget) SleeperFunc {
	args := m.MethodCalled("GetSleeper", scalingTarget)
	return args.Get(0).(SleeperFunc)
}

func (m *MockRoutes) GetMappings() map[string]string {
	args := m.MethodCalled("GetMappings")
	return args.Get(0).(map[string]string)
}

func (m *MockRoutes) GetDefaultRoute() (string, ScalingTarget, WakerFunc, SleeperFunc) {
	args := m.MethodCalled("GetDefaultRoute")
	return args.String(0), args.Get(1).(ScalingTarget), args.Get(2).(WakerFunc), args.Get(3).(SleeperFunc)
}

func (m *MockRoutes) GetAsleepMOTD(serverAddress string) string {
	args := m.MethodCalled("GetAsleepMOTD", serverAddress)
	return args.String(0)
}

func (m *MockRoutes) GetLoadingMOTD(serverAddress string) string {
	args := m.MethodCalled("GetLoadingMOTD", serverAddress)
	return args.String(0)
}

func (m *MockRoutes) SetCountdownDeadline(serverAddress string, deadline time.Time) {
	m.MethodCalled("SetCountdownDeadline", serverAddress, deadline)
}

func (m *MockRoutes) SimplifySRV(srvEnabled bool) {
	m.MethodCalled("SimplifySRV", srvEnabled)
}

func (m *MockRoutes) BulkRegister(scaler *WebhookScaler, mappings map[string]string) {
	m.MethodCalled("BulkRegister", scaler, mappings)
}

func (m *MockRoutes) WithDownScaler(downScaler IDownScaler) IRoutes {
	args := m.MethodCalled("WithDownScaler", downScaler)
	return args.Get(0).(IRoutes)
}

func (m *MockRoutes) WithListener(listener RoutesListener) IRoutes {
	args := m.MethodCalled("WithListener", listener)
	return args.Get(0).(IRoutes)
}

func Test_routesImpl_GetDefaultRouteWithoutDefaultRoute(t *testing.T) {
	// A fresh instance (no config file read, no default route set yet) must
	// not panic when the routes config loader saves on the first API route
	// creation (#603).
	routes := NewRoutes(context.Background())

	backend, scalingTarget, waker, sleeper := routes.GetDefaultRoute()
	assert.Empty(t, backend)
	assert.Nil(t, scalingTarget)
	assert.Nil(t, waker)
	assert.Nil(t, sleeper)
}

func Test_routesImpl_SetCountdownDeadlineWithoutDefaultRoute(t *testing.T) {
	routes := NewRoutes(context.Background())

	require.NotPanics(t, func() {
		routes.SetCountdownDeadline("", time.Now().Add(time.Minute))
	})
}
