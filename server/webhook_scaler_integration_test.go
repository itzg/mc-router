package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/pires/go-proxyproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webhookTestBackend observes real Minecraft traffic while ignoring the empty
// TCP connections used by the scaler's reachability checks.
type webhookTestBackend struct {
	listener     net.Listener
	observations chan webhookBackendObservation
}

type webhookBackendObservation struct {
	header *proxyproto.Header
	host   string
	player string
	err    error
}

func (b *webhookTestBackend) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		go b.observe(conn)
	}
}

func (b *webhookTestBackend) observe(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Both routes share a router, but only one backend should receive a header.
	proxyConn := proxyproto.NewConn(conn, proxyproto.WithPolicy(proxyproto.USE))
	reader := bufio.NewReader(proxyConn)
	packet, err := mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateHandshaking)
	// Reachability probes close without sending a Minecraft handshake.
	if err == io.EOF {
		return
	}
	observation := webhookBackendObservation{header: proxyConn.ProxyHeader(), err: err}
	if err == nil {
		handshake, decodeErr := mcproto.DecodeHandshake(packet.Data)
		observation.err = decodeErr
		if decodeErr == nil {
			observation.host = handshake.ServerAddress
			packet, observation.err = mcproto.ReadPacket(reader, conn.RemoteAddr(), mcproto.StateLogin)
			if observation.err == nil {
				login, loginErr := mcproto.DecodeLoginStart(handshake.ProtocolVersion, packet.Data)
				observation.err = loginErr
				if loginErr == nil {
					observation.player = login.Name
				}
			}
		}
	}
	select {
	case b.observations <- observation:
	default:
		// Unexpected extra connections must not leave a blocked observer.
		return
	}
	if observation.err == nil {
		// Keep the relay active until the client disconnects, so idle scaling can
		// be checked independently of handshake parsing.
		_, _ = io.Copy(io.Discard, proxyConn)
	}
}

func TestWebhookScalerRouteOverridesIntegration(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global PROXY enabled=%t", global), func(t *testing.T) {
			const idleDelay = 200 * time.Millisecond
			const timeout = 5 * time.Second
			type route struct {
				host, configured string
				override         bool
			}
			definitions := []route{
				{
					host:       "on.example.com",
					configured: "on.invalid:25565",
					override:   true,
				},
				{
					host:       "off.example.com",
					configured: "off.invalid:25565",
					override:   false,
				},
			}
			var mu sync.Mutex
			backends := map[string]*webhookTestBackend{}
			lastAddress := map[string]string{}
			up := make(chan WebhookScalePayload, 8)
			down := make(chan WebhookScalePayload, 8)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload WebhookScalePayload
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil {
					http.Error(w, "invalid scale request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch payload.Action {
				case scaleActionUp:
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					// Force a changed response address even if the OS reuses the old port.
					if listener.Addr().String() == lastAddress[payload.ServerAddress] {
						replacement, listenErr := net.Listen("tcp", "127.0.0.1:0")
						_ = listener.Close()
						if listenErr != nil {
							http.Error(w, listenErr.Error(), http.StatusInternalServerError)
							return
						}
						listener = replacement
					}
					lastAddress[payload.ServerAddress] = listener.Addr().String()
					backend := &webhookTestBackend{listener: listener, observations: make(chan webhookBackendObservation, 8)}
					backends[payload.ServerAddress] = backend
					go backend.serve()
					select {
					case up <- payload:
					default:
						http.Error(w, "too many scale-up requests", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(WebhookScaleResponse{Backend: listener.Addr().String()})
				case scaleActionDown:
					if backend := backends[payload.ServerAddress]; backend != nil {
						_ = backend.listener.Close()
						delete(backends, payload.ServerAddress)
					}
					select {
					case down <- payload:
					default:
						http.Error(w, "too many scale-down requests", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unknown scale action", http.StatusBadRequest)
				}
			}))
			t.Cleanup(func() {
				receiver.Close()
				mu.Lock()
				defer mu.Unlock()
				for _, backend := range backends {
					_ = backend.listener.Close()
				}
			})

			scaler := NewWebhookScaler(receiver.URL, nil, timeout, timeout)
			downScaler := NewDownScaler(true, idleDelay)
			t.Cleanup(downScaler.Reset)
			routes := NewRoutes(t.Context())
			config := RoutesConfigSchema{Mappings: map[string]RouteConfig{}}
			for _, definition := range definitions {
				config.Mappings[definition.host] = RouteConfig{Backend: definition.configured, ProxyProtocol: new(definition.override)}
			}
			data, err := json.Marshal(config)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "routes.json")
			require.NoError(t, os.WriteFile(path, data, 0600))
			require.NoError(t, NewRoutesConfigLoader(scaler, routes).Load(path))
			// Arm idle timers only after client activity, avoiding startup timers on
			// intentionally sleeping backends.
			routes.WithDownScaler(downScaler)
			connector := NewConnector(t.Context(), routes, downScaler, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
			connector.sendProxyProto = global
			listener, err := connector.createListener("127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			go connector.acceptConnections(listener, 100, 0)

			previousAddress := map[string]string{}
			for cycle := 0; cycle < 2; cycle++ {
				clients := []net.Conn{}
				for _, definition := range definitions {
					client, err := net.DialTimeout("tcp", listener.Addr().String(), timeout)
					require.NoError(t, err)
					t.Cleanup(func() { _ = client.Close() })
					clients = append(clients, client)
					require.NoError(t, client.SetDeadline(time.Now().Add(timeout)))
					require.NoError(t, writeTestPacket(client, mcproto.PacketIdHandshake, func(w io.Writer) {
						_ = mcproto.WriteVarInt(w, int32(mcproto.ProtocolVersion1_18_2))
						_ = mcproto.WriteString(w, definition.host)
						_, _ = w.Write([]byte{0x63, 0xdd})
						_ = mcproto.WriteVarInt(w, int32(mcproto.StateLogin))
					}))
					require.NoError(t, writeTestPacket(client, 0, func(w io.Writer) { _ = mcproto.WriteString(w, "ScaleTester") }))
					select {
					case payload := <-up:
						assert.Equal(t, scaleActionUp, payload.Action)
						assert.Equal(t, definition.host, payload.ServerAddress)
						assert.Equal(t, definition.configured, payload.Backend)
					case <-time.After(timeout):
						t.Fatal("scale-up webhook not received")
					}
					mu.Lock()
					backend := backends[definition.host]
					mu.Unlock()
					require.NotNil(t, backend)
					address := backend.listener.Addr().String()
					if cycle > 0 {
						assert.NotEqual(t, previousAddress[definition.host], address)
					}
					previousAddress[definition.host] = address
					select {
					case observation := <-backend.observations:
						require.NoError(t, observation.err)
						assert.Equal(t, definition.host, observation.host)
						assert.Equal(t, "ScaleTester", observation.player)
						if definition.override {
							require.NotNil(t, observation.header)
							assert.Equal(t, byte(2), observation.header.Version)
							assert.Equal(t, client.LocalAddr().String(), observation.header.SourceAddr.String())
						} else {
							assert.Nil(t, observation.header, "disabled route must receive raw Minecraft traffic")
						}
					case <-time.After(timeout):
						t.Fatal("backend did not receive Minecraft login")
					}
				}
				// Neither backend may scale down while its client remains connected.
				select {
				case payload := <-down:
					t.Fatalf("scaled down active route: %+v", payload)
				case <-time.After(2 * idleDelay):
				}
				for _, client := range clients {
					require.NoError(t, client.Close())
				}
				remaining := map[string]string{}
				for _, definition := range definitions {
					remaining[definition.host] = definition.configured
				}
				for range definitions {
					select {
					case payload := <-down:
						expected, exists := remaining[payload.ServerAddress]
						require.True(t, exists, "unexpected or duplicate scale-down: %+v", payload)
						assert.Equal(t, scaleActionDown, payload.Action)
						assert.Equal(t, expected, payload.Backend)
						// A down request must actually stop the backend listener.
						probe, dialErr := net.DialTimeout("tcp", previousAddress[payload.ServerAddress], time.Second)
						if probe != nil {
							_ = probe.Close()
						}
						assert.Error(t, dialErr, "backend still reachable after scale-down")
						delete(remaining, payload.ServerAddress)
					case <-time.After(timeout):
						t.Fatal("idle scale-down webhook not received")
					}
				}
			}
		})
	}
}
