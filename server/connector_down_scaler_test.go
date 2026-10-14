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
	routes.CreateMapping(serverAddress, backendAddress, target, nil, sleeper, "", "")

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
