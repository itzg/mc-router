package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	wakeHoldTestInterval = 50 * time.Millisecond
	// wakeHoldClientTimeout stands in for the 30 seconds a Java client waits for any packet
	wakeHoldClientTimeout = 8 * wakeHoldTestInterval
	backendGreeting       = "hello from the backend"
)

// wakeHoldEvents counts the wake hold events on top of metrics that are otherwise discarded
type wakeHoldEvents struct {
	ConnectorMetrics
	mu     sync.Mutex
	counts map[string]int
}

func (m *wakeHoldEvents) IncrementWakeHolds(event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[event]++
}

func (m *wakeHoldEvents) count(event string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[event]
}

// wakeHoldBackend is a backend that only starts listening when woken. It reports what each
// connection sent first and answers that with a disconnect carrying backendGreeting.
type wakeHoldBackend struct {
	ln         net.Listener
	wakerCalls atomic.Int32
	// release lets the waker return, as it does when the backend got reachable
	release  chan struct{}
	received chan []byte
	// wakerErr is returned by the waker, if set
	wakerErr error
}

func newWakeHoldBackend(t *testing.T) *wakeHoldBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	b := &wakeHoldBackend{ln: ln, release: make(chan struct{}), received: make(chan []byte, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(conn)
		}
	}()
	return b
}

func (b *wakeHoldBackend) serve(conn net.Conn) {
	//noinspection GoUnhandledErrorResult
	defer conn.Close()

	var received bytes.Buffer
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		received.Write(buf[:n])
		if err != nil {
			break
		}
		// whatever belongs to the login has arrived when the client goes quiet
		_ = conn.SetReadDeadline(time.Now().Add(4 * wakeHoldTestInterval))
	}
	if received.Len() == 0 {
		// a reachability check
		return
	}
	b.received <- received.Bytes()
	_ = mcproto.WriteLoginDisconnect(conn, backendGreeting)
	_ = conn.SetReadDeadline(noDeadline)
	_, _ = io.Copy(io.Discard, conn)
}

func (b *wakeHoldBackend) waker(ctx context.Context) (string, error) {
	b.wakerCalls.Add(1)
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if b.wakerErr != nil {
		return "", b.wakerErr
	}
	return b.ln.Addr().String(), nil
}

func (b *wakeHoldBackend) nextReceived(t *testing.T) []byte {
	t.Helper()
	select {
	case received := <-b.received:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was relayed to the backend")
		return nil
	}
}

// wakeHoldClient is a client that logs in and then, like the Java client, answers login plugin
// requests as not understood and gives up when nothing arrives within wakeHoldClientTimeout.
type wakeHoldClient struct {
	conn net.Conn
	// sent is the handshake and login start
	sent []byte
	// responseDelay is waited before each response
	responseDelay time.Duration
	// requests gets the message ID of each login plugin request
	requests    chan int32
	respondedAt atomic.Int64
	// ended gets the payload of the first packet that is not a login plugin request, or nil
	// when reading failed instead
	ended chan []byte
}

func startWakeHoldClient(t *testing.T, ln net.Listener, protocol mcproto.ProtocolVersion, responseDelay time.Duration) *wakeHoldClient {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	var sent bytes.Buffer
	require.NoError(t, writeTestPacket(&sent, mcproto.PacketIdHandshake, func(w io.Writer) {
		_ = mcproto.WriteVarInt(w, int32(protocol))
		_ = mcproto.WriteString(w, "mc.example.com")
		_, _ = w.Write([]byte{0x63, 0xdd})
		_ = mcproto.WriteVarInt(w, int32(mcproto.StateLogin))
	}))
	require.NoError(t, writeTestPacket(&sent, mcproto.PacketIdLogin, func(w io.Writer) {
		_ = mcproto.WriteString(w, "player1")
	}))
	_, err = conn.Write(sent.Bytes())
	require.NoError(t, err)

	c := &wakeHoldClient{
		conn:          conn,
		sent:          sent.Bytes(),
		responseDelay: responseDelay,
		requests:      make(chan int32, 1000),
		ended:         make(chan []byte, 1),
	}
	go c.run()
	return c
}

func (c *wakeHoldClient) run() {
	reader := bufio.NewReader(c.conn)
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(wakeHoldClientTimeout))
		frame, err := mcproto.ReadFrame(reader, c.conn.RemoteAddr())
		if err != nil {
			c.ended <- nil
			return
		}
		payload := bytes.NewReader(frame.Payload)
		packetID, _ := mcproto.ReadVarInt(payload)
		if packetID != mcproto.PacketIdLoginPluginRequest {
			c.ended <- frame.Payload
			return
		}
		messageID, _ := mcproto.ReadVarInt(payload)
		channel, _ := mcproto.ReadString(payload)
		if channel != "mc-router:wait" || payload.Len() != 0 {
			c.ended <- frame.Payload
			return
		}
		c.requests <- int32(messageID)

		time.Sleep(c.responseDelay)
		c.respondedAt.Store(time.Now().UnixNano())
		_ = writeTestPacket(c.conn, mcproto.PacketIdLoginPluginResponse, func(w io.Writer) {
			_ = mcproto.WriteVarInt(w, int32(messageID))
			_, _ = w.Write([]byte{0x00}) // not understood
		})
	}
}

func (c *wakeHoldClient) nextRequest(t *testing.T) int32 {
	t.Helper()
	select {
	case messageID := <-c.requests:
		return messageID
	case <-time.After(5 * time.Second):
		t.Fatal("no login plugin request arrived")
		return 0
	}
}

// disconnectReason waits for the packet that ends the hold and expects it to be a disconnect
func (c *wakeHoldClient) disconnectReason(t *testing.T) string {
	t.Helper()
	select {
	case ended := <-c.ended:
		require.NotNil(t, ended, "connection ended without a disconnect packet")
		payload := bytes.NewReader(ended)
		packetID, err := mcproto.ReadVarInt(payload)
		require.NoError(t, err)
		require.Equal(t, mcproto.PacketIdLoginDisconnect, packetID)
		reason, err := mcproto.ReadString(payload)
		require.NoError(t, err)
		var component map[string]string
		require.NoError(t, json.Unmarshal([]byte(reason), &component))
		return component["text"]
	case <-time.After(5 * time.Second):
		t.Fatal("client is still held")
		return ""
	}
}

type wakeHoldTest struct {
	connector *Connector
	backend   *wakeHoldBackend
	events    *wakeHoldEvents
	ln        net.Listener
	// scaledDown gets a value when the backend is scaled down
	scaledDown chan struct{}
}

func setupWakeHoldTest(t *testing.T, hold time.Duration, interval time.Duration) *wakeHoldTest {
	t.Helper()
	const scaleDownDelay = 300 * time.Millisecond

	backend := newWakeHoldBackend(t)
	scaledDown := make(chan struct{}, 1)
	sleeper := func(context.Context) error {
		scaledDown <- struct{}{}
		return nil
	}
	downScaler := NewDownScaler(true, scaleDownDelay)
	routes := NewRoutes(t.Context()).WithDownScaler(downScaler)
	// backend "" = scaled down
	routes.CreateMapping("mc.example.com", "", TestingScalingTarget("hold-test-backend"),
		RouteOptions{Waker: backend.waker, Sleeper: sleeper, AsleepMOTD: "asleep", LoadingMOTD: "loading"})

	events := &wakeHoldEvents{
		ConnectorMetrics: discardMetricsBuilder{}.BuildConnectorMetrics(),
		counts:           make(map[string]int),
	}
	c := NewConnector(t.Context(), routes, downScaler, events, false, false, nil, false)
	c.UseWakeKickMessage(wakeKickMessage)
	c.UseWakeHold(hold)
	c.wakeHoldInterval = interval

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go c.acceptConnections(ln, 100, 0)

	return &wakeHoldTest{connector: c, backend: backend, events: events, ln: ln, scaledDown: scaledDown}
}

func TestConnectorWakeHold(t *testing.T) {
	test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
	client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, 0)

	// The wake takes far longer than the client waits for a packet, and requests with
	// increasing message IDs, the first one right away, keep the client from giving up
	started := time.Now()
	for expected := int32(0); time.Since(started) < 3*wakeHoldClientTimeout; expected++ {
		assert.Equal(t, expected, client.nextRequest(t))
	}
	assert.Equal(t, "loading", statusDescription(t, test.ln, "mc.example.com"))
	assert.Equal(t, 1, test.events.count(wakeHoldStarted))
	assert.Zero(t, test.events.count(wakeHoldConnected))

	close(test.backend.release)

	// The backend gets the handshake and login start, and none of the client's responses
	assert.Equal(t, client.sent, test.backend.nextReceived(t))
	assert.Equal(t, backendGreeting, client.disconnectReason(t))
	assert.Equal(t, int32(1), test.backend.wakerCalls.Load())
	assert.Equal(t, 1, test.events.count(wakeHoldConnected))
	assert.Zero(t, test.events.count(wakeHoldGaveUp))
	assert.Zero(t, test.events.count(wakeHoldExpired))

	// The held login counts as a connection: only when it is gone the server scales down
	select {
	case <-test.scaledDown:
		t.Fatal("server was scaled down with a player connected")
	case <-time.After(time.Second):
	}
	_ = client.conn.Close()
	select {
	case <-test.scaledDown:
	case <-time.After(5 * time.Second):
		t.Fatal("server was never scaled down after the player left")
	}
}

// The backend would treat a response to a request it never sent as a protocol error, so the
// login is only relayed after the client answered everything it was asked.
func TestConnectorWakeHoldLateResponseDelaysRelay(t *testing.T) {
	const responseDelay = 500 * time.Millisecond

	// with this interval the hold sends its first request only
	test := setupWakeHoldTest(t, time.Minute, time.Minute)
	client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, responseDelay)

	assert.Equal(t, int32(0), client.nextRequest(t))
	close(test.backend.release)

	assert.Equal(t, client.sent, test.backend.nextReceived(t))
	respondedAt := client.respondedAt.Load()
	require.NotZero(t, respondedAt, "login was relayed with a request outstanding")
	assert.Equal(t, backendGreeting, client.disconnectReason(t))
	assert.Equal(t, 1, test.events.count(wakeHoldConnected))
}

// Clients before 1.13 have no login plugin requests, so they keep getting the kick
func TestConnectorWakeHoldKicksOldClient(t *testing.T) {
	test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
	client := startWakeHoldClient(t, test.ln, 340, 0) // 1.12.2

	assert.Equal(t, wakeKickMessage, client.disconnectReason(t))
	assert.Empty(t, client.requests)
	assert.Zero(t, test.events.count(wakeHoldStarted))
	require.Eventually(t, func() bool { return test.backend.wakerCalls.Load() == 1 }, time.Second, 10*time.Millisecond)

	// while the first 1.13 client is held
	held := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_13, 0)
	assert.Equal(t, int32(0), held.nextRequest(t))
	assert.Equal(t, 1, test.events.count(wakeHoldStarted))
}

func TestConnectorWakeHoldExpires(t *testing.T) {
	const hold = 6 * wakeHoldTestInterval

	test := setupWakeHoldTest(t, hold, wakeHoldTestInterval)
	started := time.Now()
	client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, 0)

	assert.Equal(t, wakeKickMessage, client.disconnectReason(t))
	assert.GreaterOrEqual(t, time.Since(started), hold)
	assert.NotEmpty(t, client.requests)
	assert.Equal(t, 1, test.events.count(wakeHoldExpired))
	assert.Zero(t, test.events.count(wakeHoldConnected))

	// The wake carries on for the player's next attempt
	assert.True(t, test.connector.isWakeInProgress("mc.example.com"))
	assert.Equal(t, int32(1), test.backend.wakerCalls.Load())
}

func TestConnectorWakeHoldClientLeaves(t *testing.T) {
	test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
	client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, 0)

	client.nextRequest(t)
	require.NoError(t, client.conn.Close())
	require.Eventually(t, func() bool { return test.events.count(wakeHoldGaveUp) == 1 }, 5*time.Second, 10*time.Millisecond)

	// The wake carries on and ends like one that nobody came back to after a kick
	assert.True(t, test.connector.isWakeInProgress("mc.example.com"))
	close(test.backend.release)
	select {
	case <-test.scaledDown:
	case <-time.After(5 * time.Second):
		t.Fatal("server woken for a player that left was never scheduled to scale down")
	}
	assert.False(t, test.connector.isWakeInProgress("mc.example.com"))
	assert.Equal(t, int32(1), test.backend.wakerCalls.Load())
	assert.Zero(t, test.events.count(wakeHoldConnected))
	assert.Empty(t, test.backend.received)
}

func TestConnectorWakeHoldTwoClients(t *testing.T) {
	test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
	first := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, 0)
	first.nextRequest(t)
	second := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_20_2, 0)
	second.nextRequest(t)
	assert.Equal(t, 2, test.events.count(wakeHoldStarted))

	close(test.backend.release)

	received := [][]byte{test.backend.nextReceived(t), test.backend.nextReceived(t)}
	assert.ElementsMatch(t, [][]byte{first.sent, second.sent}, received)
	assert.Equal(t, backendGreeting, first.disconnectReason(t))
	assert.Equal(t, backendGreeting, second.disconnectReason(t))
	assert.Equal(t, int32(1), test.backend.wakerCalls.Load())
	assert.Equal(t, 2, test.events.count(wakeHoldConnected))
}

// Anything but a response to one of the requests ends the hold, and the wake carries on
func TestConnectorWakeHoldUnexpectedContent(t *testing.T) {
	tests := []struct {
		name     string
		packetID int32
		payload  []byte
	}{
		{name: "response to a request never sent", packetID: mcproto.PacketIdLoginPluginResponse, payload: []byte{0x2a, 0x00}},
		{name: "other packet", packetID: 0x01, payload: []byte{0x00}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
			// too slow to respond itself before the packet below went out
			client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, time.Minute)
			client.nextRequest(t)

			require.NoError(t, writeTestPacket(client.conn, tt.packetID, func(w io.Writer) {
				_, _ = w.Write(tt.payload)
			}))

			// closed without a packet: the router no longer knows what state the client is in
			_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err := io.Copy(io.Discard, client.conn)
			assert.NoError(t, err, "connection was not closed")
			assert.True(t, test.connector.isWakeInProgress("mc.example.com"))
			assert.Zero(t, test.events.count(wakeHoldConnected))
			assert.Empty(t, test.backend.received)
		})
	}
}

func TestConnectorWakeHoldFailedWake(t *testing.T) {
	test := setupWakeHoldTest(t, time.Minute, wakeHoldTestInterval)
	test.backend.wakerErr = errors.New("timeout waiting for backend")
	client := startWakeHoldClient(t, test.ln, mcproto.ProtocolVersion1_21_5, 0)

	client.nextRequest(t)
	close(test.backend.release)

	assert.Equal(t, wakeKickMessage, client.disconnectReason(t))
	assert.Zero(t, test.events.count(wakeHoldConnected))
	assert.Zero(t, test.events.count(wakeHoldExpired))
}
