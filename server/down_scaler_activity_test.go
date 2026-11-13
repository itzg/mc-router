package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadyServerWithoutPlayersStillScalesDown(t *testing.T) {
	for _, defaultRoute := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "default"}[defaultRoute], func(t *testing.T) {
			ds := NewDownScaler(true, 20*time.Millisecond)
			t.Cleanup(ds.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(ds)
			target := TestingScalingTarget("ready-idle")
			slept := make(chan struct{}, 1)
			options := RouteOptions{Sleeper: func(context.Context) error { slept <- struct{}{}; return nil }}
			if defaultRoute {
				routes.SetDefaultRoute("", target, options)
				routes.SetDefaultRoute("backend:25565", target, options)
			} else {
				routes.CreateMapping("host", "", target, options)
				routes.UpdateMapping("host", "backend:25565", target, options)
			}
			select {
			case <-slept:
			case <-time.After(time.Second):
				t.Fatal("ready server with no players must still scale down")
			}
		})
	}
}

func TestScaleDownActivitySharedTargetAndReset(t *testing.T) {
	ds := NewDownScaler(true, time.Hour).(*downScalerImpl)
	t.Cleanup(ds.Reset)
	routes := NewRoutes(t.Context()).WithDownScaler(ds)
	target := TestingScalingTarget("shared-active")
	options := RouteOptions{Sleeper: func(context.Context) error { return nil }}
	routes.CreateMapping("first", "backend", target, options)
	ds.ConnectionOpened(target)
	ds.ConnectionOpened(target)
	// Route reload resets timers, not active connections.
	ds.Reset()
	routes.CreateMapping("second", "backend", target, options)
	routes.SetDefaultRoute("backend", target, options)
	ds.Lock()
	assert.Empty(t, ds.timers)
	ds.Unlock()
	ds.ConnectionClosed(target)
	ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
	ds.Lock()
	assert.Empty(t, ds.timers, "one player remains on this target")
	ds.Unlock()
	ds.ConnectionClosed(target)
	ds.Start(t.Context(), target, routes, routes.GetScaleDownPolicy(target))
	ds.Lock()
	assert.NotNil(t, ds.timers[target.ScalingKey()])
	assert.Empty(t, ds.active, "completed connections must not leak target counts")
	ds.Unlock()
}

type blockedSleeperRoutes struct {
	IRoutes
	resolving chan struct{}
	release   chan struct{}
	calls     atomic.Int32
}

func (r *blockedSleeperRoutes) GetSleeper(target ScalingTarget) SleeperFunc {
	if r.calls.Add(1) == 1 {
		close(r.resolving)
		<-r.release
	}
	return r.IRoutes.GetSleeper(target)
}

func TestCancelledScaleDownCallbackCannotReplaceNewTimer(t *testing.T) {
	ds := NewDownScaler(true, 10*time.Millisecond).(*downScalerImpl)
	t.Cleanup(ds.Reset)
	routes := NewRoutes(t.Context())
	target := TestingScalingTarget("cancelled-expiry")
	slept := make(chan struct{}, 1)
	routes.CreateMapping("host", "backend", target, RouteOptions{Sleeper: func(context.Context) error { slept <- struct{}{}; return nil }})
	blocked := &blockedSleeperRoutes{IRoutes: routes, resolving: make(chan struct{}), release: make(chan struct{})}
	// Release a stuck callback even if the test fails before the explicit close.
	defer func() {
		select {
		case <-blocked.release:
		default:
			close(blocked.release)
		}
	}()
	ds.Start(t.Context(), target, blocked, ScaleDownPolicy{})
	select {
	case <-blocked.resolving:
	case <-time.After(time.Second):
		t.Fatal("timer callback did not start")
	}
	ds.ConnectionOpened(target) // Timer.Stop can no longer prevent this callback.
	ds.ConnectionClosed(target)
	ds.Start(t.Context(), target, blocked, ScaleDownPolicy{longestOverride: time.Hour})
	ds.Lock()
	replacement := ds.timers[target.ScalingKey()]
	ds.Unlock()
	require.NotNil(t, replacement)
	close(blocked.release)
	select {
	case <-slept:
		t.Fatal("cancelled callback scaled down after the player returned")
	case <-time.After(50 * time.Millisecond):
	}
	ds.Lock()
	assert.Same(t, replacement, ds.timers[target.ScalingKey()], "stale callback must not erase the replacement timer")
	ds.Unlock()
}
