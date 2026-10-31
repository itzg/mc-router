package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wakeKickMessage = "Server is starting, try again in 30 seconds"

// dialAndHandshake opens a client connection to the connector and sends a handshake
func dialAndHandshake(t *testing.T, ln net.Listener, serverAddress string, nextState mcproto.State) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, writeTestPacket(conn, mcproto.PacketIdHandshake, func(w io.Writer) {
		_ = mcproto.WriteVarInt(w, int32(mcproto.ProtocolVersion1_18_2))
		_ = mcproto.WriteString(w, serverAddress)
		_, _ = w.Write([]byte{0x63, 0xdd})
		_ = mcproto.WriteVarInt(w, int32(nextState))
	}))
	return conn
}

func loginAndReadDisconnect(t *testing.T, ln net.Listener, serverAddress string) string {
	t.Helper()
	conn := dialAndHandshake(t, ln, serverAddress, mcproto.StateLogin)
	require.NoError(t, writeTestPacket(conn, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "player1")
	}))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := mcproto.ReadFrame(bufio.NewReader(conn), conn.RemoteAddr())
	require.NoError(t, err, "expected a disconnect packet instead of a held connection")

	payload := bytes.NewReader(frame.Payload)
	packetID, err := mcproto.ReadVarInt(payload)
	require.NoError(t, err)
	require.Equal(t, mcproto.PacketIdLoginDisconnect, packetID)
	reason, err := mcproto.ReadString(payload)
	require.NoError(t, err)

	var component map[string]string
	require.NoError(t, json.Unmarshal([]byte(reason), &component))
	return component["text"]
}

func statusDescription(t *testing.T, ln net.Listener, serverAddress string) string {
	t.Helper()
	conn := dialAndHandshake(t, ln, serverAddress, mcproto.StateStatus)
	require.NoError(t, writeTestPacket(conn, mcproto.PacketIdStatusRequest, nil))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := mcproto.ReadFrame(bufio.NewReader(conn), conn.RemoteAddr())
	require.NoError(t, err, "expected a status response")
	payload := bytes.NewReader(frame.Payload)
	_, err = mcproto.ReadVarInt(payload)
	require.NoError(t, err)
	jsonStr, err := mcproto.ReadString(payload)
	require.NoError(t, err)

	var status struct {
		Description struct {
			Text string `json:"text"`
		} `json:"description"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonStr), &status))
	return status.Description.Text
}

func TestConnectorWakeKick(t *testing.T) {
	const scaleDownDelay = 300 * time.Millisecond

	downScaler := NewDownScaler(true, scaleDownDelay)
	routes := NewRoutes(t.Context()).WithDownScaler(downScaler)
	target := TestingScalingTarget("kick-test-backend")

	releaseWaker := make(chan struct{})
	var wakerCalls atomic.Int32
	waker := func(ctx context.Context) (string, error) {
		wakerCalls.Add(1)
		select {
		case <-releaseWaker:
		case <-ctx.Done():
		}
		return "127.0.0.1:1", nil
	}
	scaleDownCalled := make(chan struct{}, 1)
	sleeper := func(context.Context) error {
		scaleDownCalled <- struct{}{}
		return nil
	}
	// backend "" = scaled down
	routes.CreateMapping("mc.example.com", "", target, waker, sleeper, "asleep", "loading")

	c := NewConnector(t.Context(), routes, downScaler, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	c.UseWakeKickMessage(wakeKickMessage)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go c.acceptConnections(ln, 100, 0)

	// A login via an SRV record arrives as a fully qualified name with a trailing dot
	assert.Equal(t, wakeKickMessage, loginAndReadDisconnect(t, ln, "MC.example.com."))
	require.Eventually(t, func() bool { return wakerCalls.Load() == 1 }, time.Second, 10*time.Millisecond)

	// While waking, status pings with and without the trailing dot both see the loading MOTD
	assert.Equal(t, "loading", statusDescription(t, ln, "mc.example.com."))
	assert.Equal(t, "loading", statusDescription(t, ln, "mc.example.com"))

	// A second login during the wake is told the same and does not start another wake
	assert.Equal(t, wakeKickMessage, loginAndReadDisconnect(t, ln, "mc.example.com"))
	assert.Equal(t, int32(1), wakerCalls.Load())

	// Nobody returns after the wake, so the scale-down timer must still be armed
	close(releaseWaker)
	select {
	case <-scaleDownCalled:
	case <-time.After(scaleDownDelay * 10):
		t.Fatal("server woken in the background was never scheduled to scale down")
	}
	assert.False(t, c.isWakeInProgress("mc.example.com"))
}

func TestConnectorWakeKickDisabledHoldsLogin(t *testing.T) {
	routes := NewRoutes(t.Context())
	target := TestingScalingTarget("hold-test-backend")

	wakerCalled := make(chan struct{}, 1)
	releaseWaker := make(chan struct{})
	waker := func(ctx context.Context) (string, error) {
		wakerCalled <- struct{}{}
		select {
		case <-releaseWaker:
		case <-ctx.Done():
		}
		return "127.0.0.1:1", nil
	}
	routes.CreateMapping("mc.example.com", "", target, waker, nil, "asleep", "loading")

	c := NewConnector(t.Context(), routes, nil, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go c.acceptConnections(ln, 100, 0)

	conn := dialAndHandshake(t, ln, "mc.example.com", mcproto.StateLogin)
	require.NoError(t, writeTestPacket(conn, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "player1")
	}))
	select {
	case <-wakerCalled:
	case <-time.After(time.Second):
		t.Fatal("login did not wake the backend")
	}

	// Without a kick message the login is held: nothing is written to the client
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err = mcproto.ReadFrame(bufio.NewReader(conn), conn.RemoteAddr())
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "expected the login to be held, got %v", err)
	close(releaseWaker)
}

func TestConnectorDoesNotWakeRoutableBackend(t *testing.T) {
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

	var wakeCalls atomic.Int32
	target := TestingScalingTarget("already-running")
	routes := NewRoutes(t.Context())
	routes.CreateMapping("mc.example.com", backendListener.Addr().String(), target, func(context.Context) (string, error) {
		wakeCalls.Add(1)
		return backendListener.Addr().String(), nil
	}, nil, "", "")

	connector := NewConnector(t.Context(), routes, nil, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go connector.acceptConnections(listener, 100, 0)

	client := dialAndHandshake(t, listener, "mc.example.com", mcproto.StateLogin)
	require.NoError(t, writeTestPacket(client, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "player1")
	}))

	select {
	case conn := <-backendConnection:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("login did not connect to the routable backend")
	}
	assert.Zero(t, wakeCalls.Load(), "routable backend should not invoke its waker")
}
