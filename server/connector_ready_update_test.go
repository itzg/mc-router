package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reproduces a provider's delayed ready event after a successful wake/login.
// Uses the inherited delay; the late-ready case also fails before #563.
func TestConnectorReadyUpdateOrder(t *testing.T) {
	tests := []struct {
		name            string
		readyDuringWake bool
	}{
		{
			name: "ready after login",
		},
		{
			name:            "ready during wake and after login",
			readyDuringWake: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const idleDelay = 80 * time.Millisecond
			const host = "ready.example.com"
			f := setupConnectorIdleTest(t, idleDelay)
			options := f.readyDuringWakeOptions(host, tt.readyDuringWake)
			f.routes.CreateMapping(host, "", f.target, options)

			// Admission must cancel any timer created by readiness during wake.
			client := f.login(host)
			f.assertActiveWithoutTimer(1)
			require.False(t, f.target.IsScaling(), "wake guard must have cleared")

			// A later discovery event must not rearm shutdown for this active player.
			f.routes.UpdateMapping(host, f.backend.Addr().String(), f.target, options)
			f.assertAwake(3 * idleDelay)

			// Once the player leaves, shutdown must still be allowed.
			require.NoError(t, client.Close())
			f.assertSleepsWithin(time.Second)
		})
	}
}

// readyDuringWakeOptions optionally publishes readiness while the wake guard is held.
func (f *connectorIdleTest) readyDuringWakeOptions(host string, readyDuringWake bool) RouteOptions {
	var options RouteOptions
	options = RouteOptions{
		Waker: func(context.Context) (string, error) {
			if readyDuringWake {
				f.routes.UpdateMapping(host, f.backend.Addr().String(), f.target, options)
			}
			return f.backend.Addr().String(), nil
		},
		Sleeper: f.recordSleep,
	}
	return options
}
