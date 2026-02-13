package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
			r := NewRoutes()

			r.CreateMapping(tt.mapping.serverAddress, tt.mapping.backend, "", nil, nil, "")

			if got, server, _, _, _ := r.FindBackendForServerAddress(context.Background(), tt.args.serverAddress); got != tt.want {
				t.Errorf("routesImpl.FindBackendForServerAddress() = %v, want %v", got, tt.want)
			} else {
				assert.Equal(t, tt.mapping.serverAddress, server)
			}
		})
	}
}

func Test_routesImpl_ScaleKey(t *testing.T) {
	DownScaler = NewDownScaler(context.Background(), false, 1*time.Second)

	t.Run("scaleKey defaults to backend when empty", func(t *testing.T) {
		r := NewRoutes()
		r.CreateMapping("mc.example.com", "backend:25565", "", nil, nil, "")

		_, _, scaleKey, _, _ := r.FindBackendForServerAddress(context.Background(), "mc.example.com")
		assert.Equal(t, "backend:25565", scaleKey)
	})

	t.Run("scaleKey is set when provided", func(t *testing.T) {
		r := NewRoutes()
		r.CreateMapping("mc.example.com", "proxy:25577", "10.0.0.5:25565", nil, nil, "")

		backend, _, scaleKey, _, _ := r.FindBackendForServerAddress(context.Background(), "mc.example.com")
		assert.Equal(t, "proxy:25577", backend)
		assert.Equal(t, "10.0.0.5:25565", scaleKey)
	})

	t.Run("GetSleepers matches on scaleKey not backend", func(t *testing.T) {
		r := NewRoutes()
		called := false
		sleeper := func(ctx context.Context) error {
			called = true
			return nil
		}

		// Two routes with same proxy backend but different scaleKeys
		r.CreateMapping("mc1.example.com", "proxy:25577", "10.0.0.1:25565", nil, sleeper, "")
		r.CreateMapping("mc2.example.com", "proxy:25577", "10.0.0.2:25565", nil, nil, "")

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
		r := NewRoutes()
		r.SetDefaultRoute("proxy:25577", "10.0.0.5:25565", nil, nil, "")

		backend, scaleKey, _, _ := r.GetDefaultRoute()
		assert.Equal(t, "proxy:25577", backend)
		assert.Equal(t, "10.0.0.5:25565", scaleKey)
	})

	t.Run("default route scaleKey defaults to backend", func(t *testing.T) {
		r := NewRoutes()
		r.SetDefaultRoute("backend:25565", "", nil, nil, "")

		backend, scaleKey, _, _ := r.GetDefaultRoute()
		assert.Equal(t, "backend:25565", backend)
		assert.Equal(t, "backend:25565", scaleKey)
	})
}
