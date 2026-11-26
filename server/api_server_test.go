package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_apiServer_routesListHandler(t *testing.T) {
	tests := []struct {
		name          string
		scalingTarget ScalingTarget
		proxyProtocol *bool
		want          string
	}{
		{
			name:          "with scaling target",
			scalingTarget: &testingScalingTarget{name: "sc1"},
			want:          `{"mc.example.com":{"backend":"backend:25565","scalingTarget":"sc1"}}`,
		},
		{
			name: "nil scaling target",
			want: `{"mc.example.com":{"backend":"backend:25565","scalingTarget":""}}`,
		},
		{
			name:          "PROXY protocol enabled",
			proxyProtocol: new(true),
			want:          `{"mc.example.com":{"backend":"backend:25565","scalingTarget":"","proxyProtocol":true}}`,
		},
		{
			name:          "PROXY protocol disabled",
			proxyProtocol: new(false),
			want:          `{"mc.example.com":{"backend":"backend:25565","scalingTarget":"","proxyProtocol":false}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := new(MockRoutes)
			mappings := map[string]RouteConfig{}
			// Preserve explicit true and false overrides; nil must omit the API field.
			mappings["mc.example.com"] = RouteConfig{Backend: "backend:25565", ProxyProtocol: tt.proxyProtocol}

			routes.On("GetMappings").Return(mappings)
			routes.On("FindBackendForServerAddress", mock.Anything, "mc.example.com").Return("backend:25565", "mc.example.com",
				tt.scalingTarget, nillableWaker(nil), nillableSleeper(nil), tt.proxyProtocol)
			server, err := StartApiServer(t.Context(), ApiConfig{Binding: "127.0.0.1:0"}, routes, nil, nil)
			if err != nil {
				require.NotNil(t, err)
			}

			serverAddr := server.GetAddr()
			// get listing by http request to serverAddr

			//goland:noinspection HttpUrlsUsage
			resp, err := http.Get("http://" + serverAddr + "/routes")
			require.NoError(t, err)
			//goland:noinspection GoUnhandledErrorResult
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(body))
		})
	}
}

func Test_apiServer_closesIncompleteHeaders(t *testing.T) {
	server, err := StartApiServer(t.Context(), ApiConfig{Binding: "127.0.0.1:0", Key: "test-key"}, new(MockRoutes), nil, nil)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", server.GetAddr())
	require.NoError(t, err)
	//goland:noinspection GoUnhandledErrorResult
	defer conn.Close()

	// Send a request line and one header, but never the blank line that ends the headers.
	_, err = conn.Write([]byte("GET /routes HTTP/1.1\r\nHost: localhost\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(handshakeTimeout+5*time.Second)))
	_, err = io.ReadAll(conn)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("API server kept a connection with incomplete headers open")
	}
}
