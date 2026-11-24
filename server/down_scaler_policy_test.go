package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScaleDownPolicyBelongsToResource(t *testing.T) {
	tests := []struct {
		name         string
		defaultFirst bool
	}{
		{name: "named aliases"},
		{
			name:         "default and named aliases",
			defaultFirst: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			target := TestingScalingTarget("shared")
			target.SetScaleDownPolicy(ScaleDownPolicy{delay: time.Hour})
			if tt.defaultFirst {
				routes.SetDefaultRoute("proxy:25565", target, RouteOptions{})
			}
			routes.CreateMapping("first", "proxy:25565", target, RouteOptions{})

			// Rebuilt discovery objects reference one canonical resource and policy.
			rebuilt := TestingScalingTarget("shared")
			rebuilt.SetScaleDownPolicy(ScaleDownPolicy{delay: time.Hour})
			routes.CreateMapping("second", "other-proxy:25565", rebuilt, RouteOptions{})
			_, _, first, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "first")
			_, _, second, _, _, _ := routes.FindBackendForServerAddress(t.Context(), "second")
			require.Same(t, first, second)
			assert.Equal(t, time.Hour, second.ScaleDownPolicy().resolve(time.Minute))

			// One resource update affects every alias without reconciling per-route values.
			rebuilt.SetScaleDownPolicy(ScaleDownPolicy{delay: 5 * time.Minute})
			routes.UpdateMapping("second", "other-proxy:25565", rebuilt, RouteOptions{})
			assert.Equal(t, 5*time.Minute, first.ScaleDownPolicy().resolve(time.Minute))
			routes.RemoveMapping("second")
			assert.Equal(t, 5*time.Minute, first.ScaleDownPolicy().resolve(time.Minute))

			// Clearing resource metadata restores inheritance for all remaining routes.
			rebuilt.SetScaleDownPolicy(ScaleDownPolicy{})
			routes.UpdateMapping("first", "proxy:25565", rebuilt, RouteOptions{})
			assert.Equal(t, time.Minute, first.ScaleDownPolicy().resolve(time.Minute))
			if tt.defaultFirst {
				_, fallback, _, _ := routes.GetDefaultRoute()
				assert.Same(t, first, fallback)
			}
		})
	}
}

func TestScaleDownOverrideScheduling(t *testing.T) {
	for _, path := range []string{"registration", "default", "ready", "direct scheduler start"} {
		t.Run(path, func(t *testing.T) {
			ds := NewDownScaler(true, time.Hour)
			t.Cleanup(ds.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(ds)
			target := TestingScalingTarget("target")
			target.SetScaleDownPolicy(ScaleDownPolicy{delay: 20 * time.Millisecond})
			slept := make(chan struct{}, 1)
			options := RouteOptions{Sleeper: func(context.Context) error { slept <- struct{}{}; return nil }}
			switch path {
			case "registration":
				routes.CreateMapping("host", "backend", target, options)
			case "default":
				routes.SetDefaultRoute("backend", target, options)
			case "ready":
				routes.CreateMapping("host", "", target, options)
				routes.UpdateMapping("host", "backend", target, options)
			case "direct scheduler start":
				// Exercises DownScaler directly; Connector lifecycle coverage lives
				// in the disconnect and background-wake tests.
				routes.CreateMapping("host", "", target, options)
				ds.Start(t.Context(), target, routes)
			}
			select {
			case <-slept:
			case <-time.After(2 * time.Second):
				t.Fatal("override was not used by scheduler")
			}
		})
	}
}

func TestScaleDownPolicyUpdatePreservesTimerState(t *testing.T) {
	ds := NewDownScaler(true, time.Hour).(*downScalerImpl)
	t.Cleanup(ds.Reset)
	routes := NewRoutes(t.Context()).WithDownScaler(ds)
	target := TestingScalingTarget("target")
	target.SetScaleDownPolicy(ScaleDownPolicy{delay: time.Hour})
	options := RouteOptions{Sleeper: func(context.Context) error { return nil }}
	routes.CreateMapping("host", "backend", target, options)
	ds.Lock()
	original := ds.timers[target.ScalingKey()]
	ds.Unlock()
	require.NotNil(t, original)

	// Updating metadata changes future policy, not the timer already pending.
	target.SetScaleDownPolicy(ScaleDownPolicy{delay: 2 * time.Hour})
	routes.UpdateMapping("host", "backend", target, options)
	ds.Start(t.Context(), target, routes)
	ds.Lock()
	assert.Same(t, original, ds.timers[target.ScalingKey()])
	ds.Unlock()
	assert.Equal(t, 2*time.Hour, target.ScaleDownPolicy().resolve(time.Minute))

	// A qualifying active connection has cancelled the timer. A metadata update
	// must not recreate it. The next idle cycle explicitly calls Start.
	ds.Cancel(target)
	target.SetScaleDownPolicy(ScaleDownPolicy{delay: 3 * time.Hour})
	routes.UpdateMapping("host", "backend", target, options)
	routes.CreateMapping("host", "backend", target, options)
	ds.Lock()
	assert.Empty(t, ds.timers)
	ds.Unlock()

	// Starting the next idle cycle may create a new timer with the updated policy.
	ds.Start(t.Context(), target, routes)
	ds.Lock()
	assert.NotNil(t, ds.timers[target.ScalingKey()])
	ds.Unlock()
}
