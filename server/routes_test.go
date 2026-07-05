package server

import (
	"context"
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

			r.CreateMapping(tt.mapping.serverAddress, tt.mapping.backend, "", nil, nil, "", "", 0)

			if got, server, _, _, _ := r.FindBackendForServerAddress(context.Background(), tt.args.serverAddress); got != tt.want {
				t.Errorf("routesImpl.FindBackendForServerAddress() = %v, want %v", got, tt.want)
			} else {
				assert.Equal(t, tt.mapping.serverAddress, server)
			}
		})
	}
}

func Test_routesImpl_ScaleKey(t *testing.T) {
	downScaler := NewDownScaler(false, 1*time.Second)

	t.Run("scaleKey defaults to backend when empty", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "", 0)

		_, _, scaleKey, _, _ := r.FindBackendForServerAddress(context.Background(), "mc.example.com")
		assert.Equal(t, "backend:25565", scaleKey)
	})

	t.Run("scaleKey is set when provided", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.CreateMapping("mc.example.com", "proxy:25577", "10.0.0.5:25565", nil, nil, "", "", 0)

		backend, _, scaleKey, _, _ := r.FindBackendForServerAddress(context.Background(), "mc.example.com")
		assert.Equal(t, "proxy:25577", backend)
		assert.Equal(t, "10.0.0.5:25565", scaleKey)
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
		r.CreateMapping("mc1.example.com", "proxy:25577", "10.0.0.1:25565", nil, sleeper, "", "", 0)
		r.CreateMapping("mc2.example.com", "proxy:25577", "10.0.0.2:25565", nil, nil, "", "", 0)

		sleepers := r.GetSleepers("10.0.0.1:25565")
		require.Len(t, sleepers, 1)
		_ = sleepers[0](context.Background())
		assert.True(t, called)

		// No sleeper for the second scaleKey since it has nil sleeper
		sleepers = r.GetSleepers("10.0.0.2:25565")
		assert.Empty(t, sleepers)

		// No sleeper when querying by proxy backend address
		sleepers = r.GetSleepers("proxy:25577")
		assert.Empty(t, sleepers)
	})

	t.Run("default route scaleKey", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.SetDefaultRoute("proxy:25577", "10.0.0.5:25565", nil, nil, "", "", 0)

		backend, scaleKey, _, _ := r.GetDefaultRoute()
		assert.Equal(t, "proxy:25577", backend)
		assert.Equal(t, "10.0.0.5:25565", scaleKey)
	})

	t.Run("default route scaleKey defaults to backend", func(t *testing.T) {
		r := NewRoutes(t.Context())
		r.WithDownScaler(downScaler)
		r.SetDefaultRoute("backend:25565", "", nil, nil, "", "", 0)

		backend, scaleKey, _, _ := r.GetDefaultRoute()
		assert.Equal(t, "backend:25565", backend)
		assert.Equal(t, "backend:25565", scaleKey)
	})
}

func Test_routesImpl_LoadingMOTD(t *testing.T) {
	r := NewRoutes(t.Context())
	r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "asleep", "loading", 0)

	assert.Equal(t, "loading", r.GetLoadingMOTD("mc.example.com"))
	assert.Equal(t, "", r.GetLoadingMOTD("other.example.com"))

	r.SetDefaultRoute("default:25565", "", nil, nil, "default asleep", "default loading", 0)
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

	r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "", 0)

	listener.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")
}

func TestRoutesListener_OnRouteRemoved(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnRouteAdded", "mc.example.com", "backend:25565").Return()
	listener.On("OnRouteRemoved", "mc.example.com").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "", 0)
	listener.AssertCalled(t, "OnRouteAdded", "mc.example.com", "backend:25565")

	r.DeleteMapping("mc.example.com")
	listener.AssertCalled(t, "OnRouteRemoved", "mc.example.com")
}

func TestRoutesListener_OnDefaultRouteAdded(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnDefaultRouteSet", "default:25565").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)

	r.SetDefaultRoute("default:25565", "", nil, nil, "", "", 0)

	listener.AssertCalled(t, "OnDefaultRouteSet", "default:25565")
}

func TestRoutesListener_OnDefaultRouteRemoved_dueToReset(t *testing.T) {
	listener := &mockRoutesListener{}
	listener.On("OnDefaultRouteSet", "default:25565").Return()
	listener.On("OnDefaultRouteRemoved").Return()
	r := NewRoutes(t.Context()).
		WithListener(listener)
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.SetDefaultRoute("default:25565", "", nil, nil, "", "", 0)
	listener.AssertCalled(t, "OnDefaultRouteSet", "default:25565")

	r.Reset()
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

	r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "", 0)

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

	r.CreateMapping("mc1.example.com", "backend:25565", "", nil, nil, "", "", 0)
	r.CreateMapping("mc2.example.com", "backend:25566", "", nil, nil, "", "", 0)
	r.CreateMapping("mc3.example.com", "backend:25567", "", nil, nil, "", "", 0)

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

	deleted := r.DeleteMapping("nonexistent.example.com")
	assert.False(t, deleted)

	listener.AssertNotCalled(t, "OnRouteRemoved", "nonexistent.example.com")
}

func TestRoutesListener_NilListenersHandled(t *testing.T) {
	r := NewRoutes(t.Context())
	r.WithDownScaler(NewDownScaler(false, 5*time.Second))

	r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "", "", 0)
	r.SetDefaultRoute("default:25565", "", nil, nil, "", "", 0)
	r.DeleteMapping("mc.example.com")
	r.Reset()
}
