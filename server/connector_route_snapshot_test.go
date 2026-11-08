package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/pires/go-proxyproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectorRetainsRouteSnapshotDuringWake(t *testing.T) {
	const serverAddress = "snapshot.example.com"
	const timeout = 3 * time.Second

	for _, test := range []struct {
		name                   string
		oldOverride            *bool
		newOverride            *bool
		global                 bool
		expectUsesDefaultRoute bool
		wantOldHeader          bool
		wantNewHeader          bool
	}{
		{
			name:          "disabled to enabled",
			oldOverride:   new(false),
			newOverride:   new(true),
			wantNewHeader: true,
		},
		{
			name:          "enabled to disabled",
			oldOverride:   new(true),
			newOverride:   new(false),
			wantOldHeader: true,
		},
		{
			name:          "inherit global enabled to disabled",
			newOverride:   new(false),
			global:        true,
			wantOldHeader: true,
		},
		{
			name:          "inherit global disabled to enabled",
			newOverride:   new(true),
			wantNewHeader: true,
		},
		{
			name:                   "default route to named route",
			oldOverride:            new(false),
			newOverride:            new(true),
			expectUsesDefaultRoute: true,
			wantNewHeader:          true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Separate listeners make backend selection observable independently of
			// header selection. The waker returns the old route's live address.
			listen := func() *net.TCPListener {
				listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				return listener
			}
			oldBackend := listen()
			newBackend := listen()
			wakeStarted := make(chan struct{})
			releaseWake := make(chan struct{})
			waker := func(ctx context.Context) (string, error) {
				close(wakeStarted)
				select {
				case <-releaseWake:
					return oldBackend.Addr().String(), nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}

			routes := NewRoutes(t.Context())
			options := RouteOptions{Waker: waker, ProxyProtocol: test.oldOverride}
			target := TestingScalingTarget("snapshot-backend")
			if test.expectUsesDefaultRoute {
				routes.SetDefaultRoute("configured.invalid:25565", target, options)
			} else {
				routes.CreateMapping(serverAddress, "configured.invalid:25565", target, options)
			}
			connector := NewConnector(t.Context(), routes, nil, discardMetricsBuilder{}.BuildConnectorMetrics(), false, false, nil, false)
			connector.sendProxyProto = test.global
			frontend := listen()
			go connector.acceptConnections(frontend, 100, 0)

			login := func() net.Conn {
				client := dialAndHandshake(t, frontend, serverAddress, mcproto.StateLogin)
				require.NoError(t, client.SetDeadline(time.Now().Add(timeout)))
				require.NoError(t, writeTestPacket(client, mcproto.PacketIdLogin, func(w io.Writer) {
					_ = mcproto.WriteString(w, "SnapshotTester")
				}))
				return client
			}
			oldClient := login()
			select {
			case <-wakeStarted:
			case <-time.After(timeout):
				t.Fatal("initial connection did not reach the blocked waker")
			}

			// Update after route selection but before backend dialing. The in-flight
			// connection must retain the selected override even across a slow wake.
			newOptions := RouteOptions{ProxyProtocol: test.newOverride}
			if test.expectUsesDefaultRoute {
				routes.CreateMapping(serverAddress, newBackend.Addr().String(), nil, newOptions)
			} else {
				routes.UpdateMapping(serverAddress, newBackend.Addr().String(), nil, newOptions)
			}
			close(releaseWake)

			checkBackend := func(listener *net.TCPListener, client net.Conn, wantHeader bool) {
				t.Helper()
				require.NoError(t, listener.SetDeadline(time.Now().Add(timeout)))
				backend, err := listener.Accept()
				require.NoError(t, err)
				t.Cleanup(func() { _ = backend.Close() })
				proxy := proxyproto.NewConn(backend, proxyproto.WithPolicy(proxyproto.USE))
				require.NoError(t, proxy.SetReadDeadline(time.Now().Add(timeout)))
				header := proxy.ProxyHeader()
				if wantHeader {
					require.NotNil(t, header)
					assert.Equal(t, byte(2), header.Version)
					assert.Equal(t, client.LocalAddr().String(), header.SourceAddr.String())
				} else {
					assert.Nil(t, header)
				}
				reader := bufio.NewReader(proxy)
				packet, err := mcproto.ReadPacket(reader, backend.RemoteAddr(), mcproto.StateHandshaking)
				require.NoError(t, err)
				require.Equal(t, mcproto.PacketIdHandshake, packet.PacketID)
				handshake, err := mcproto.DecodeHandshake(packet.Data)
				require.NoError(t, err)
				assert.Equal(t, serverAddress, handshake.ServerAddress)
				packet, err = mcproto.ReadPacket(reader, backend.RemoteAddr(), mcproto.StateLogin)
				require.NoError(t, err)
				assert.Equal(t, mcproto.PacketIdLogin, packet.PacketID)
			}
			checkBackend(oldBackend, oldClient, test.wantOldHeader)

			// New connections should observe the replacement route and its override.
			newClient := login()
			checkBackend(newBackend, newClient, test.wantNewHeader)
		})
	}
}
