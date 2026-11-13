package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/require"
)

func TestConnectorBackgroundWakeIdleOverride(t *testing.T) {
	const idleDelay = 300 * time.Millisecond
	const host = "background.example.com"
	for _, returns := range []bool{false, true} {
		t.Run(map[bool]string{false: "no returning player", true: "returning player cancels timer"}[returns], func(t *testing.T) {
			ds := NewDownScaler(true, time.Hour).(*downScalerImpl)
			t.Cleanup(ds.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(ds)
			target := TestingScalingTarget("background-override")
			connector := NewConnector(t.Context(), routes, ds, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
			connector.UseWakeKickMessage(wakeKickMessage)

			backend, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = backend.Close() })
			releaseWake := make(chan struct{})
			wakeStarted := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-releaseWake:
				default:
					close(releaseWake)
				}
			})
			var wakeCalls atomic.Int32
			type sleepEvent struct {
				at     time.Time
				active int
			}
			slept := make(chan sleepEvent, 1)
			var options RouteOptions
			options = RouteOptions{
				AutoScaleDownAfter: new(idleDelay),
				Waker: func(ctx context.Context) (string, error) {
					if wakeCalls.Add(1) == 1 {
						close(wakeStarted)
						select {
						case <-releaseWake:
						case <-ctx.Done():
							return "", ctx.Err()
						}
					}
					// Simulate discovery catching up during wake, while the scaling guard
					// is held. That update must not be the source of the idle timer.
					routes.UpdateMapping(host, backend.Addr().String(), target, options)
					return backend.Addr().String(), nil
				},
				Sleeper: func(context.Context) error {
					slept <- sleepEvent{at: time.Now(), active: connector.scaleActiveConnections.GetCount(target.ScalingKey())}
					return nil
				},
			}
			routes.CreateMapping(host, "", target, options)
			frontend, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = frontend.Close() })
			go connector.acceptConnections(frontend, 100, 0)
			require.Equal(t, wakeKickMessage, loginAndReadDisconnect(t, frontend, host))
			select {
			case <-wakeStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("kicked login did not start a background wake")
			}
			require.True(t, connector.isWakeInProgress(host))
			require.Zero(t, connector.scaleActiveConnections.GetCount(target.ScalingKey()))
			timerPending := func() bool {
				ds.Lock()
				defer ds.Unlock()
				return ds.timers[target.ScalingKey()] != nil
			}
			require.False(t, timerPending(), "sleeping route must not schedule before wake completes")
			releasedAt := time.Now()
			close(releaseWake)

			if !returns {
				select {
				case event := <-slept:
					require.Zero(t, event.active)
					require.GreaterOrEqual(t, event.at.Sub(releasedAt), idleDelay, "background wake must use the route delay after completion")
				case <-time.After(idleDelay + 2*time.Second):
					t.Fatal("successful background wake did not schedule the override timeout")
				}
				require.False(t, connector.isWakeInProgress(host))
				return
			}

			require.Eventually(t, func() bool { return !connector.isWakeInProgress(host) && timerPending() }, 2*time.Second, time.Millisecond, "background wake must arm an idle timer before the player returns")
			client := dialAndHandshake(t, frontend, host, mcproto.StateLogin)
			require.NoError(t, writeTestPacket(client, mcproto.PacketIdLogin, func(w io.Writer) { _ = mcproto.WriteString(w, "ReturningPlayer") }))
			require.NoError(t, backend.(*net.TCPListener).SetDeadline(time.Now().Add(2*time.Second)))
			for {
				conn, err := backend.Accept()
				require.NoError(t, err)
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
				reader := bufio.NewReader(conn)
				_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateHandshaking)
				if errors.Is(err, io.EOF) {
					// Wake-kick readiness checks connect without sending a handshake.
					_ = conn.Close()
					continue
				}
				t.Cleanup(func() { _ = conn.Close() })
				require.NoError(t, err)
				_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateLogin)
				require.NoError(t, err)
				break
			}
			require.Equal(t, 1, connector.scaleActiveConnections.GetCount(target.ScalingKey()))
			require.False(t, timerPending(), "returning player's login must cancel the background idle timer")
			select {
			case event := <-slept:
				t.Fatalf("background timer fired with %d active player(s)", event.active)
			case <-time.After(2 * idleDelay):
			}
			disconnectedAt := time.Now()
			require.NoError(t, client.Close())
			select {
			case event := <-slept:
				require.Zero(t, event.active)
				require.GreaterOrEqual(t, event.at.Sub(disconnectedAt), idleDelay, "last disconnect must start a fresh override timeout")
			case <-time.After(idleDelay + 2*time.Second):
				t.Fatal("returning player's final disconnect did not schedule scale-down")
			}
		})
	}
}
