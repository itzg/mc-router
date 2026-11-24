package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/require"
)

func TestConnectorScaleDownDelayAfterDisconnect(t *testing.T) {
	const idleDelay = 400 * time.Millisecond
	tests := []struct {
		name     string
		global   time.Duration
		override *time.Duration
	}{
		{
			name:   "inherited",
			global: idleDelay,
		},
		{
			name:     "shorter override",
			global:   time.Hour,
			override: new(idleDelay),
		},
		{
			name:     "longer override",
			global:   50 * time.Millisecond,
			override: new(idleDelay),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setupConnectorIdleTest(t, tt.global)
			if tt.override != nil {
				f.target.SetScaleDownPolicy(ScaleDownPolicy{delay: *tt.override})
			}
			options := RouteOptions{Sleeper: f.recordSleep}
			// Both hostnames share one target and therefore one connection count and timer.
			f.routes.CreateMapping("first.example.com", f.backend.Addr().String(), f.target, options)
			f.routes.CreateMapping("second.example.com", f.backend.Addr().String(), f.target, options)

			// Establish one player, then disconnect to create the timer that reconnect must cancel.
			first := f.login("first.example.com")
			f.assertActiveWithoutTimer(1)
			require.NoError(t, first.Close())
			f.assertTimerPending()

			// Returning players cancel that timer, including when they use different aliases.
			second := f.login("second.example.com")
			third := f.login("first.example.com")
			f.assertActiveWithoutTimer(2)
			f.assertAwake(2 * idleDelay)

			// One player leaving must not schedule shutdown while the other remains.
			require.NoError(t, second.Close())
			f.assertActiveWithoutTimer(1)
			f.assertAwake(2 * idleDelay)

			// Only the final disconnect starts the full effective idle delay.
			lastDisconnect := time.Now()
			require.NoError(t, third.Close())
			f.assertSleepsAfter(lastDisconnect, idleDelay)
		})
	}
}

// connectorIdleTest keeps TCP and timer mechanics out of the lifecycle scenarios.
// It uses the real Connector, routes, and DownScaler; it does not simulate their behavior.
type connectorIdleTest struct {
	t         *testing.T
	ds        *downScalerImpl
	routes    IRoutes
	target    ScalingTarget
	connector *Connector
	backend   *net.TCPListener
	frontend  *net.TCPListener
	slept     chan connectorSleepEvent
}

type connectorSleepEvent struct {
	at     time.Time
	active int
}

func setupConnectorIdleTest(t *testing.T, globalDelay time.Duration) *connectorIdleTest {
	t.Helper()
	ds := NewDownScaler(true, globalDelay).(*downScalerImpl)
	t.Cleanup(ds.Reset)
	routes := NewRoutes(t.Context()).WithDownScaler(ds)
	f := &connectorIdleTest{
		t:      t,
		ds:     ds,
		routes: routes,
		target: TestingScalingTarget(t.Name()),
		slept:  make(chan connectorSleepEvent, 8),
	}
	f.connector = NewConnector(t.Context(), routes, ds, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	f.backend = f.listen()
	f.frontend = f.listen()
	go f.connector.acceptConnections(f.frontend, 100, 0)
	return f
}

func (f *connectorIdleTest) listen() *net.TCPListener {
	f.t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func (f *connectorIdleTest) recordSleep(context.Context) error {
	f.slept <- connectorSleepEvent{
		at:     time.Now(),
		active: f.connector.scaleActiveConnections.GetCount(f.target.ScalingKey()),
	}
	return nil
}

// login waits for the forwarded packets, which confirms Connector has admitted the player.
func (f *connectorIdleTest) login(host string) net.Conn {
	f.t.Helper()
	client := dialAndHandshake(f.t, f.frontend, host, mcproto.StateLogin)
	require.NoError(f.t, writeTestPacket(client, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "IdleDelayTest")
	}))
	f.assertLoginForwarded()
	return client
}

func (f *connectorIdleTest) assertLoginForwarded() {
	f.t.Helper()
	require.NoError(f.t, f.backend.SetDeadline(time.Now().Add(2*time.Second)))
	for {
		conn, err := f.backend.Accept()
		require.NoError(f.t, err, "login did not reach the backend")
		f.t.Cleanup(func() { _ = conn.Close() })
		require.NoError(f.t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		reader := bufio.NewReader(conn)
		_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateHandshaking)
		if errors.Is(err, io.EOF) {
			// Wake-kick readiness checks connect without sending a handshake.
			_ = conn.Close()
			continue
		}
		require.NoError(f.t, err)
		_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateLogin)
		require.NoError(f.t, err)
		return
	}
}

func (f *connectorIdleTest) timerPending() bool {
	f.ds.Lock()
	defer f.ds.Unlock()
	return f.ds.timers[f.target.ScalingKey()] != nil
}

func (f *connectorIdleTest) assertTimerPending() {
	f.t.Helper()
	require.Eventually(f.t, f.timerPending, 2*time.Second, 5*time.Millisecond, "idle timer was not scheduled")
}

func (f *connectorIdleTest) assertActiveWithoutTimer(count int) {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		return f.connector.scaleActiveConnections.GetCount(f.target.ScalingKey()) == count && !f.timerPending()
	}, 2*time.Second, 5*time.Millisecond, "active players must cancel the idle timer")
}

func (f *connectorIdleTest) assertAwake(duration time.Duration) {
	f.t.Helper()
	select {
	case event := <-f.slept:
		f.t.Fatalf("scaled down with %d active connections", event.active)
	case <-time.After(duration):
	}
}

func (f *connectorIdleTest) assertSleepsWithin(timeout time.Duration) connectorSleepEvent {
	f.t.Helper()
	select {
	case event := <-f.slept:
		require.Zero(f.t, event.active, "shutdown must not run with active players")
		return event
	case <-time.After(timeout):
		f.t.Fatal("idle server did not scale down")
		return connectorSleepEvent{}
	}
}

func (f *connectorIdleTest) assertSleepsAfter(since time.Time, delay time.Duration) {
	f.t.Helper()
	event := f.assertSleepsWithin(delay + 2*time.Second)
	require.GreaterOrEqual(f.t, event.at.Sub(since), delay, "shutdown must wait for the full idle delay")
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
