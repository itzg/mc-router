package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScaleDownDelaySharedTargets(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			routes := NewRoutes(t.Context())
			target := TestingScalingTarget("shared")
			sleeper := func(context.Context) error { return nil }
			aliases := []struct {
				host  string
				delay *time.Duration
			}{
				{"long", new(12 * time.Hour)}, {"short", new(5 * time.Minute)}, {"inherit", nil},
			}
			for i := range aliases {
				if reverse {
					i = len(aliases) - 1 - i
				}
				alias := aliases[i]
				routes.CreateMapping(alias.host, "backend", target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: alias.delay})
			}
			// Another target sharing the proxy backend must not affect this policy.
			routes.CreateMapping("other", "backend", TestingScalingTarget("other"), RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: new(24 * time.Hour)})
			// A route without a sleeper does not veto another alias's scaling policy.
			routes.CreateMapping("unscaled", "backend", target, RouteOptions{AutoScaleDownAfter: new(48 * time.Hour)})
			assert.Equal(t, 12*time.Hour, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))
			routes.RemoveMapping("long")
			assert.Equal(t, 10*time.Minute, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))
			routes.RemoveMapping("inherit")
			assert.Equal(t, 5*time.Minute, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))
			routes.SetDefaultRoute("backend", target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: new(time.Hour)})
			assert.Equal(t, time.Hour, routes.GetScaleDownPolicy(target).resolve(10*time.Minute))
			assert.Equal(t, 10*time.Minute, routes.GetScaleDownPolicy(nil).resolve(10*time.Minute))
		})
	}
}

func TestScaleDownOverrideScheduling(t *testing.T) {
	for _, path := range []string{"registration", "default", "ready", "disconnect or background wake"} {
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
			default:
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
	ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
	ds.Lock()
	assert.NotNil(t, ds.timers[target.ScalingKey()])
	ds.Unlock()
}
