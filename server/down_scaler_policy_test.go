package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScaleDownDelaySharedTargets(t *testing.T) {
	tests := []struct {
		name    string
		reverse bool
	}{
		{
			name: "forward",
		},
		{
			name:    "reverse",
			reverse: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := NewRoutes(t.Context())
			target := TestingScalingTarget("shared")
			sleeper := func(context.Context) error { return nil }
			aliases := []struct {
				host  string
				delay *time.Duration
			}{
				{
					host:  "long",
					delay: new(12 * time.Hour),
				},
				{
					host:  "short",
					delay: new(5 * time.Minute),
				},
				{
					host:  "inherit",
					delay: nil,
				},
			}
			// Each case owns a fresh slice; reverse it to verify registration-order independence.
			if tt.reverse {
				slices.Reverse(aliases)
			}
			for _, alias := range aliases {
				routes.CreateMapping(alias.host, "backend", target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: alias.delay})
			}
			// Another target sharing the proxy backend must not affect this policy.
			routes.CreateMapping("other", "backend", TestingScalingTarget("other"), RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: new(24 * time.Hour)})
			// A route without a sleeper does not veto another alias's scaling policy.
			routes.CreateMapping("unscaled", "backend", target, RouteOptions{AutoScaleDownAfter: new(48 * time.Hour)})

			// The longest explicit override wins over the inherited ten-minute delay.
			assert.Equal(t, 12*time.Hour, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

			// Removing twelve hours leaves inheritance longer than the five-minute override.
			routes.RemoveMapping("long")
			assert.Equal(t, 10*time.Minute, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

			// With no inheriting alias, only the five-minute override remains.
			routes.RemoveMapping("inherit")
			assert.Equal(t, 5*time.Minute, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

			// Default routes participate in the same target-wide policy.
			routes.SetDefaultRoute("backend", target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: new(time.Hour)})
			assert.Equal(t, time.Hour, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))

			// Without a scaling target, policy resolution falls back to the global delay.
			assert.Equal(t, 10*time.Minute, routes.GetScaleDownPolicy(nil).resolve(10*time.Minute))
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
			slept := make(chan struct{}, 1)
			options := RouteOptions{Sleeper: func(context.Context) error { slept <- struct{}{}; return nil }, AutoScaleDownAfter: new(20 * time.Millisecond)}
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
				ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
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
	options := RouteOptions{Sleeper: func(context.Context) error { return nil }, AutoScaleDownAfter: new(time.Hour)}
	routes.CreateMapping("host", "backend", target, options)
	ds.Lock()
	original := ds.timers[target.ScalingKey()]
	ds.Unlock()
	require.NotNil(t, original)

	// Updating metadata changes future policy, not the timer already pending.
	options.AutoScaleDownAfter = new(2 * time.Hour)
	routes.UpdateMapping("host", "backend", target, options)
	ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
	ds.Lock()
	assert.Same(t, original, ds.timers[target.ScalingKey()])
	ds.Unlock()
	assert.Equal(t, 2*time.Hour, routes.GetScaleDownPolicy(target).resolve(time.Minute))

	// A qualifying active connection has cancelled the timer. A metadata update
	// must not recreate it. The next idle cycle explicitly calls Start.
	ds.Cancel(target)
	options.AutoScaleDownAfter = new(3 * time.Hour)
	routes.UpdateMapping("host", "backend", target, options)
	routes.CreateMapping("host", "backend", target, options)
	ds.Lock()
	assert.Empty(t, ds.timers)
	ds.Unlock()

	// Starting the next idle cycle may create a new timer with the updated policy.
	ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
	ds.Lock()
	assert.NotNil(t, ds.timers[target.ScalingKey()])
	ds.Unlock()
}
