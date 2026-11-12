package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/require"
)

func TestConnectorScaleDownDelayAfterDisconnect(t *testing.T) {
	const idleDelay = 400 * time.Millisecond
	for _, tt := range []struct {
		name     string
		global   time.Duration
		override *time.Duration
	}{
		{name: "inherited", global: idleDelay},
		{name: "shorter override", global: time.Hour, override: new(idleDelay)},
		{name: "longer override", global: 50 * time.Millisecond, override: new(idleDelay)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			downScaler := NewDownScaler(true, tt.global).(*downScalerImpl)
			t.Cleanup(downScaler.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(downScaler)
			target := TestingScalingTarget("disconnect-test-backend")
			connector := NewConnector(t.Context(), routes, downScaler, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
			type sleepEvent struct {
				at     time.Time
				active int
			}
			slept := make(chan sleepEvent, 8)
			options := RouteOptions{
				AutoScaleDownAfter: tt.override,
				Sleeper: func(context.Context) error {
					slept <- sleepEvent{at: time.Now(), active: connector.scaleActiveConnections.GetCount(target.ScalingKey())}
					return nil
				},
			}

			backend, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = backend.Close() })
			accepted := make(chan net.Conn, 3)
			go func() {
				for range 3 {
					conn, err := backend.Accept()
					if err != nil {
						return
					}
					accepted <- conn
				}
			}()
			frontend, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = frontend.Close() })
			go connector.acceptConnections(frontend, 100, 0)
			// Both aliases share scaling identity, regardless of the hostname used
			// by the last player to leave.
			for _, host := range []string{"first.example.com", "second.example.com"} {
				routes.CreateMapping(host, backend.Addr().String(), target, options)
			}
			timerPending := func() bool {
				downScaler.Lock()
				defer downScaler.Unlock()
				return downScaler.timers[target.ScalingKey()] != nil
			}
			login := func(host string) net.Conn {
				t.Helper()
				conn := dialAndHandshake(t, frontend, host, mcproto.StateLogin)
				require.NoError(t, writeTestPacket(conn, mcproto.PacketIdLogin, func(w io.Writer) {
					_ = mcproto.WriteString(w, "IdleDelayTest")
				}))
				select {
				case backendConn := <-accepted:
					t.Cleanup(func() { _ = backendConn.Close() })
					require.NoError(t, backendConn.SetReadDeadline(time.Now().Add(2*time.Second)))
					reader := bufio.NewReader(backendConn)
					_, err := mcproto.ReadPacket(reader, backendConn.RemoteAddr(), mcproto.StateHandshaking)
					require.NoError(t, err)
					_, err = mcproto.ReadPacket(reader, backendConn.RemoteAddr(), mcproto.StateLogin)
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("login did not reach the backend")
				}
				return conn
			}
			assertAwake := func() {
				t.Helper()
				select {
				case event := <-slept:
					t.Fatalf("scaled down with %d active connections", event.active)
				case <-time.After(2 * idleDelay):
				}
			}

			first := login("first.example.com")
			require.Eventually(t, func() bool {
				return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 1 && !timerPending()
			}, 2*time.Second, 5*time.Millisecond, "login must cancel the startup timer")
			require.NoError(t, first.Close())
			require.Eventually(t, timerPending, 2*time.Second, 5*time.Millisecond, "disconnect must arm an idle timer")

			// A returning player must cancel the disconnect timer. Keep two
			// players connected through different aliases to exercise target counts.
			second := login("second.example.com")
			third := login("first.example.com")
			require.Eventually(t, func() bool {
				return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 2 && !timerPending()
			}, 2*time.Second, 5*time.Millisecond, "returning players must cancel the idle timer")
			assertAwake()
			require.NoError(t, second.Close())
			require.Eventually(t, func() bool {
				return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 1
			}, 2*time.Second, 5*time.Millisecond)
			require.False(t, timerPending(), "one remaining player must prevent idle scheduling")
			assertAwake()

			lastDisconnect := time.Now()
			require.NoError(t, third.Close())
			select {
			case event := <-slept:
				require.Zero(t, event.active)
				require.GreaterOrEqual(t, event.at.Sub(lastDisconnect), idleDelay, "idle delay must start after the final disconnect")
			case <-time.After(idleDelay + 2*time.Second):
				t.Fatal("final disconnect did not scale down using the effective delay")
			}
		})
	}
}

func TestStatusRequestDownScalerTimer(t *testing.T) {
	for _, resetTimerOnStatus := range []bool{false, true} {
		name := "does not reset timer by default"
		if resetTimerOnStatus {
			name = "resets timer when configured"
		}
		t.Run(name, func(t *testing.T) {
			testStatusRequestDownScalerTimer(t, resetTimerOnStatus)
		})
	}
}

func testStatusRequestDownScalerTimer(t *testing.T, resetTimerOnStatus bool) {
	const scaleDownDelay = 2 * time.Second
	const statusConnectionCloseTimeout = 2 * time.Second
	const statusResponseReadTimeout = 2 * time.Second
	const serverAddress = "mc.example.com"

	// Configure a scalable route and record when its sleeper runs.
	downScaler := NewDownScaler(true, scaleDownDelay)
	routes := NewRoutes(t.Context()).WithDownScaler(downScaler)
	target := TestingScalingTarget("status-test-backend")
	scaleDownCalled := make(chan struct{}, 1)
	sleeper := func(context.Context) error {
		scaleDownCalled <- struct{}{}
		return nil
	}

	// Start the backend and router listeners used to observe the proxied status exchange.
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = backendListener.Close() })

	backendConnection := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := backendListener.Accept()
		if acceptErr == nil {
			backendConnection <- conn
		}
	}()

	connector := NewConnector(
		t.Context(),
		routes,
		downScaler,
		discardMetricsBuilder{}.BuildConnectorMetrics(),
		false,
		false,
		nil,
		resetTimerOnStatus,
	)

	frontendListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = frontendListener.Close() })
	go connector.acceptConnections(frontendListener, 100, 0)

	backendAddress := backendListener.Addr().String()
	timerStartedAt := time.Now()
	routes.CreateMapping(serverAddress, backendAddress, target, RouteOptions{Sleeper: sleeper})

	// Open a frontend connection, then wait until halfway through the idle delay
	// before sending a status request.
	clientConnection, err := net.Dial("tcp", frontendListener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientConnection.Close() })
	time.Sleep(scaleDownDelay / 2)

	require.NoError(t, writeTestPacket(clientConnection, mcproto.PacketIdHandshake, func(w io.Writer) {
		_ = mcproto.WriteVarInt(w, int32(mcproto.ProtocolVersion1_18_2))
		_ = mcproto.WriteString(w, serverAddress)
		_, _ = w.Write([]byte{0x63, 0xdd})
		_ = mcproto.WriteVarInt(w, int32(mcproto.StateStatus))
	}))
	require.NoError(t, writeTestPacket(clientConnection, mcproto.PacketIdStatusRequest, nil))

	var acceptedBackendConnection net.Conn
	select {
	case acceptedBackendConnection = <-backendConnection:
		t.Cleanup(func() { _ = acceptedBackendConnection.Close() })
	case <-time.After(2 * time.Second):
		t.Fatal("router did not connect to test backend")
	}

	// Verify the router forwards the handshake and status request, then relay a response.
	backendReader := bufio.NewReader(acceptedBackendConnection)
	handshakePacket, err := mcproto.ReadPacket(backendReader, acceptedBackendConnection.RemoteAddr(), mcproto.StateHandshaking)
	require.NoError(t, err)
	require.Equal(t, mcproto.PacketIdHandshake, handshakePacket.PacketID)

	statusPacket, err := mcproto.ReadPacket(backendReader, acceptedBackendConnection.RemoteAddr(), mcproto.StateStatus)
	require.NoError(t, err)
	require.Equal(t, mcproto.PacketIdStatusRequest, statusPacket.PacketID)
	require.Less(t, time.Since(timerStartedAt), scaleDownDelay,
		"status request reached backend after the scale-down timer had already expired")
	require.NoError(t, writeTestPacket(acceptedBackendConnection, mcproto.PacketIdStatusResponse, func(w io.Writer) {
		_ = mcproto.WriteString(w, "{}")
	}))

	require.NoError(t, clientConnection.SetReadDeadline(time.Now().Add(statusResponseReadTimeout)))
	_, err = mcproto.ReadFrame(bufio.NewReader(clientConnection), clientConnection.RemoteAddr())
	require.NoError(t, err)
	require.NoError(t, clientConnection.Close())
	require.Eventually(t, func() bool {
		return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 0
	}, statusConnectionCloseTimeout, 10*time.Millisecond, "status connection did not close")

	// Status must not move the original deadline by default; when enabled, it
	// resets the timer on connect and scale-down occurs only after that delay.
	if resetTimerOnStatus {
		// The original deadline must pass without a scale-down; the status
		// request has moved the timer later.
		select {
		case <-scaleDownCalled:
			t.Fatal("scale-down ran before the configured status timer reset could expire")
		case <-time.After(time.Until(timerStartedAt.Add(scaleDownDelay + scaleDownDelay/4))):
		}

		// Once the reset delay has elapsed, the sleeper should run.
		select {
		case <-scaleDownCalled:
		case <-time.After(scaleDownDelay):
			t.Fatal("scale-down did not run after the configured status timer reset expired")
		}
	} else {
		// Without the option, the sleeper must run by the original deadline.
		select {
		case <-scaleDownCalled:
		case <-time.After(time.Until(timerStartedAt.Add(scaleDownDelay + scaleDownDelay/4))):
			t.Fatal("status request postponed scale-down beyond the original timer")
		}
	}
}
