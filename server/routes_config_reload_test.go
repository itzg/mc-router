package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/require"
)

func TestRouteConfigReloadDoesNotScaleDownActiveConnection(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "override"}[override], func(t *testing.T) {
			testRouteConfigReloadDoesNotScaleDownActiveConnection(t, override)
		})
	}
}

func testRouteConfigReloadDoesNotScaleDownActiveConnection(t *testing.T, override bool) {
	const scaleDownDelay = 500 * time.Millisecond
	const serverAddress = "mc.example.com"

	globalDelay := scaleDownDelay
	var downAfter *time.Duration
	if override {
		globalDelay = time.Hour
		downAfter = new(scaleDownDelay)
	}
	downScaler := NewDownScaler(true, globalDelay)
	routes := NewRoutes(t.Context()).WithDownScaler(downScaler)
	target := TestingScalingTarget("reload-test-backend")
	var connector *Connector
	scaleDownCalled := make(chan int, 1)
	sleeper := func(context.Context) error {
		scaleDownCalled <- connector.scaleActiveConnections.GetCount(target.ScalingKey())
		return nil
	}

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

	backendAddress := backendListener.Addr().String()
	routes.CreateMapping(serverAddress, backendAddress, target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: downAfter})

	connector = NewConnector(
		t.Context(),
		routes,
		downScaler,
		discardMetricsBuilder{}.BuildConnectorMetrics(),
		false,
		false,
		nil,
		false,
	)

	frontendListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = frontendListener.Close() })
	go connector.acceptConnections(frontendListener, 100, 0)

	clientConnection, err := net.Dial("tcp", frontendListener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientConnection.Close() })

	require.NoError(t, writeTestPacket(clientConnection, 0x00, func(w io.Writer) {
		_ = mcproto.WriteVarInt(w, int32(mcproto.ProtocolVersion1_18_2))
		_ = mcproto.WriteString(w, serverAddress)
		_, _ = w.Write([]byte{0x63, 0xdd})
		_ = mcproto.WriteVarInt(w, int32(mcproto.StateLogin))
	}))
	require.NoError(t, writeTestPacket(clientConnection, 0x00, func(w io.Writer) {
		_ = mcproto.WriteString(w, "RouteReloadTest")
	}))

	var acceptedBackendConnection net.Conn
	select {
	case acceptedBackendConnection = <-backendConnection:
		t.Cleanup(func() { _ = acceptedBackendConnection.Close() })
	case <-time.After(2 * time.Second):
		t.Fatal("router did not connect to test backend")
	}

	require.Eventually(t, func() bool {
		return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 1
	}, 2*time.Second, 10*time.Millisecond, "test connection never became active")

	// RoutesConfigLoader.Load calls BulkRegister after a watched file change,
	// which calls CreateMapping again for this unchanged, already-active route.
	routes.CreateMapping(serverAddress, backendAddress, target, RouteOptions{Sleeper: sleeper, AutoScaleDownAfter: downAfter})

	select {
	case activeConnections := <-scaleDownCalled:
		t.Fatalf("scale-down sleeper ran with %d active backend connection(s)", activeConnections)
	case <-time.After(2 * scaleDownDelay):
	}
}
