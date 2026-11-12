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

// Reproduces a provider's delayed ready event after a successful wake/login.
// Uses the inherited delay; the late-ready case also fails before #563.
func TestReadyUpdateMustNotScaleDownActivePlayer(t *testing.T) {
	testReadyUpdateOrder(t, false)
}
func TestReadyUpdateDuringWakePreservesActivePlayer(t *testing.T) {
	testReadyUpdateOrder(t, true)
}
func testReadyUpdateOrder(t *testing.T, readyDuringWake bool) {
	const idleDelay = 80 * time.Millisecond
	ds := NewDownScaler(true, idleDelay)
	t.Cleanup(ds.Reset)
	routes := NewRoutes(t.Context()).WithDownScaler(ds)
	target := TestingScalingTarget("delayed-ready")
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := backend.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	connector := NewConnector(t.Context(), routes, ds, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	slept := make(chan int, 1)
	var options RouteOptions
	options = RouteOptions{
		Waker: func(context.Context) (string, error) {
			if readyDuringWake {
				routes.UpdateMapping("ready.example.com", backend.Addr().String(), target, options)
			}
			return backend.Addr().String(), nil
		},
		Sleeper: func(context.Context) error {
			slept <- connector.scaleActiveConnections.GetCount(target.ScalingKey())
			return nil
		},
	}
	routes.CreateMapping("ready.example.com", "", target, options)
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = frontend.Close() })
	go connector.acceptConnections(frontend, 100, 0)
	client := dialAndHandshake(t, frontend, "ready.example.com", mcproto.StateLogin)
	require.NoError(t, writeTestPacket(client, mcproto.PacketIdLogin, func(w io.Writer) { _ = mcproto.WriteString(w, "ReadyRace") }))
	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		reader := bufio.NewReader(conn)
		// Forwarding occurs after Connector registers activity and cancels its
		// timer, so the delayed event below cannot race ahead of that step.
		_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateHandshaking)
		require.NoError(t, err)
		_, err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateLogin)
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("no backend connection")
	}
	require.Eventually(t, func() bool { return connector.scaleActiveConnections.GetCount(target.ScalingKey()) == 1 }, time.Second, time.Millisecond)
	require.False(t, target.IsScaling(), "wake guard must have cleared")
	// Discovery has caught up after login cancelled its down timer.
	routes.UpdateMapping("ready.example.com", backend.Addr().String(), target, options)
	select {
	case active := <-slept:
		t.Fatalf("ready update scaled down with %d active player(s)", active)
	case <-time.After(3 * idleDelay):
	}
	require.NoError(t, client.Close())
	select {
	case active := <-slept:
		require.Zero(t, active, "final disconnect must still allow scale-down")
	case <-time.After(time.Second):
		t.Fatal("idle server did not scale down after final disconnect")
	}
}
