package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	routes.CreateMapping("mc.example.com", "", target, RouteOptions{Waker: waker, Sleeper: sleeper, AsleepMOTD: "asleep", LoadingMOTD: "loading"})

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
	routes.CreateMapping("mc.example.com", "", target, RouteOptions{Waker: waker, AsleepMOTD: "asleep", LoadingMOTD: "loading"})

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

// closedPort returns a loopback address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// Static routes (-mapping, -default, routes config) scaled through the webhook always carry their
// configured backend, so an empty backend cannot tell that the server is asleep. The kick must
// still reach the first player when that backend refuses connections.
func TestConnectorWakeKickStaticBackendAsleep(t *testing.T) {
	routes := NewRoutes(t.Context())
	target := TestingScalingTarget("static-backend")
	backend := closedPort(t)

	releaseWaker := make(chan struct{})
	t.Cleanup(func() { close(releaseWaker) })
	var wakerCalls atomic.Int32
	waker := func(ctx context.Context) (string, error) {
		wakerCalls.Add(1)
		select {
		case <-releaseWaker:
		case <-ctx.Done():
		}
		return backend, nil
	}
	routes.CreateMapping("mc.example.com", backend, target, RouteOptions{Waker: waker, AsleepMOTD: "asleep", LoadingMOTD: "loading"})

	c := NewConnector(t.Context(), routes, nil, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	c.UseWakeKickMessage(wakeKickMessage)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go c.acceptConnections(ln, 100, 0)

	assert.Equal(t, wakeKickMessage, loginAndReadDisconnect(t, ln, "mc.example.com"))
	require.Eventually(t, func() bool { return wakerCalls.Load() == 1 }, time.Second, 10*time.Millisecond)

	// A second login during the same wake is kicked too and starts no second wake
	assert.Equal(t, wakeKickMessage, loginAndReadDisconnect(t, ln, "mc.example.com"))
	assert.Equal(t, int32(1), wakerCalls.Load())
}

// A static backend that accepts connections is relayed as before: no kick, no wake.
func TestConnectorWakeKickStaticBackendAwake(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = backendLn.Close() })
	handshakes := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1)
				if n, _ := conn.Read(buf); n > 0 {
					handshakes <- struct{}{}
				}
				_, _ = io.Copy(io.Discard, conn) // stay open like a real server
			}(conn)
		}
	}()

	routes := NewRoutes(t.Context())
	var wakerCalls atomic.Int32
	waker := func(ctx context.Context) (string, error) {
		wakerCalls.Add(1)
		return backendLn.Addr().String(), nil
	}
	routes.CreateMapping("mc.example.com", backendLn.Addr().String(), TestingScalingTarget("awake-backend"), RouteOptions{Waker: waker, AsleepMOTD: "asleep", LoadingMOTD: "loading"})

	c := NewConnector(t.Context(), routes, nil, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
	c.UseWakeKickMessage(wakeKickMessage)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go c.acceptConnections(ln, 100, 0)

	conn := dialAndHandshake(t, ln, "mc.example.com", mcproto.StateLogin)
	require.NoError(t, writeTestPacket(conn, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "player1")
	}))
	select {
	case <-handshakes:
	case <-time.After(2 * time.Second):
		t.Fatal("login was not relayed to the reachable backend")
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = mcproto.ReadFrame(bufio.NewReader(conn), conn.RemoteAddr())
	var netErr net.Error
	require.ErrorAs(t, err, &netErr, "a reachable backend must not be answered with a kick")
}

func TestConnectorBackgroundWakeIdleOverride(t *testing.T) {
	const idleDelay = 300 * time.Millisecond
	const host = "background.example.com"
	tests := []struct {
		name    string
		returns bool
	}{
		{
			name: "no returning player",
		},
		{
			name:    "returning player cancels timer",
			returns: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a long global delay so shutdown proves that the route override was used.
			ds := NewDownScaler(true, time.Hour).(*downScalerImpl)
			t.Cleanup(ds.Reset)
			routes := NewRoutes(t.Context()).WithDownScaler(ds)
			target := TestingScalingTarget("background-override")
			connector := NewConnector(t.Context(), routes, ds, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
			connector.UseWakeKickMessage(wakeKickMessage)

			// Block the first wake until the kicked login has left without an active player.
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
			// Record shutdown time and activity independently of the wake completion signal.
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

			// A real login must be kicked while the wake continues in the background.
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

			// Completing a successful wake must arm the idle timer even without a player.
			releasedAt := time.Now()
			close(releaseWake)

			if !tt.returns {
				// Without a returning player, shutdown must follow the override delay.
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

			// Wait for the background timer before reconnecting, so cancellation is observable.
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

			// The forwarded login confirms admission; the target must stay awake past the old deadline.
			require.Equal(t, 1, connector.scaleActiveConnections.GetCount(target.ScalingKey()))
			require.False(t, timerPending(), "returning player's login must cancel the background idle timer")
			select {
			case event := <-slept:
				t.Fatalf("background timer fired with %d active player(s)", event.active)
			case <-time.After(2 * idleDelay):
			}

			// The final disconnect starts a fresh full delay rather than reviving the old timer.
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
