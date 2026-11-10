package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteOverridesFileAndAPI(t *testing.T) {
	for _, scaled := range []bool{false, true} {
		name := "unscaled"
		if scaled {
			name = "webhook scaled"
		}
		t.Run(name, func(t *testing.T) {
			var scaler *WebhookScaler
			if scaled {
				scaler = NewWebhookScaler("http://localhost/scale", nil, time.Second, time.Second)
			}
			routes := NewRoutes(t.Context())
			loader := NewRoutesConfigLoader(scaler, routes)
			path := filepath.Join(t.TempDir(), "routes.json")
			require.NoError(t, os.WriteFile(path, []byte(`{"default-server":{"backend":"default:25565","proxy-protocol":false},"mappings":{"off.example.com":{"backend":"off:25565","proxy-protocol":false},"on.example.com":{"backend":"on:25565","proxy-protocol":true},"inherit.example.com":"inherit:25565"}}`), 0600))
			require.NoError(t, loader.Load(path))
			api := &ApiServer{ctx: t.Context(), routes: routes, configLoader: loader, webhookScaler: scaler}
			for _, tt := range []struct {
				path, body string
				handler    http.HandlerFunc
			}{
				{"/routes", `{"serverAddress":"added.example.com","backend":"added:25565","proxyProtocol":false}`, api.routesCreateHandler},
				{"/defaultRoute", `{"backend":"default:25565","proxyProtocol":true}`, api.routesSetDefault},
			} {
				resp := httptest.NewRecorder()
				tt.handler(resp, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)))
				require.Less(t, resp.Code, 300, resp.Body.String())
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var config RoutesConfigSchema
				require.NoError(t, json.Unmarshal(data, &config))
				assert.Equal(t, new(false), config.Mappings["off.example.com"].ProxyProtocol)
				assert.Equal(t, new(false), config.Mappings["added.example.com"].ProxyProtocol)
				assert.Equal(t, new(true), config.Mappings["on.example.com"].ProxyProtocol)
				assert.Nil(t, config.Mappings["inherit.example.com"].ProxyProtocol)
				if tt.path == "/routes" {
					assert.Equal(t, new(false), config.DefaultServer.ProxyProtocol)
				} else {
					assert.Equal(t, new(true), config.DefaultServer.ProxyProtocol)
				}
				require.NoError(t, loader.Reload())
			}
			assert.Equal(t, new(true), routes.GetProxyProtocol("unknown"))
			assert.Nil(t, routes.GetProxyProtocol("inherit.example.com"))
			for _, host := range []string{"off.example.com", "added.example.com", "unknown"} {
				_, _, target, waker, sleeper, _ := routes.FindBackendForServerAddress(t.Context(), host)
				if scaled {
					assert.NotNil(t, target)
					assert.NotNil(t, waker)
					assert.NotNil(t, sleeper)
				} else {
					assert.Nil(t, waker)
					assert.Nil(t, sleeper)
				}
			}
			listing := httptest.NewRecorder()
			api.routesListHandler(listing, httptest.NewRequest(http.MethodGet, "/routes", nil))
			var listed map[string]struct {
				ProxyProtocol *bool `json:"proxyProtocol"`
			}
			require.NoError(t, json.Unmarshal(listing.Body.Bytes(), &listed))
			assert.Equal(t, new(false), listed["off.example.com"].ProxyProtocol)
			assert.Equal(t, new(true), listed["on.example.com"].ProxyProtocol)
			assert.Nil(t, listed["inherit.example.com"].ProxyProtocol)
			// Omitting the API override clears the previous value and persists inheritance.
			api.routesCreateHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/routes", strings.NewReader(`{"serverAddress":"off.example.com","backend":"off:25565"}`)))
			api.routesSetDefault(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/defaultRoute", strings.NewReader(`{"backend":"default:25565","proxyProtocol":null}`)))
			require.NoError(t, loader.Reload())
			assert.Nil(t, routes.GetProxyProtocol("off.example.com"))
			assert.Nil(t, routes.GetProxyProtocol("unknown"))
			// Reloading legacy strings also clears overrides and removes old mappings.
			require.NoError(t, os.WriteFile(path, []byte(`{"default-server":"default:25565","mappings":{"on.example.com":"on:25565"}}`), 0600))
			require.NoError(t, loader.Reload())
			assert.Nil(t, routes.GetProxyProtocol("on.example.com"))
			assert.Len(t, routes.GetMappings(), 1)
		})
	}
}

func TestLegacyRoutesConfigRoundTrip(t *testing.T) {
	for _, input := range []string{
		`{"default-server":"default:25565","mappings":{"a.example.com":"a:25565","b.example.com":"b:25565"}}`,
		`{"default-server":"","mappings":{"a.example.com":"a:25565"}}`,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.json")
			require.NoError(t, os.WriteFile(path, []byte(input), 0600))
			loader := NewRoutesConfigLoader(nil, NewRoutes(t.Context()))
			require.NoError(t, loader.Load(path))
			loader.SaveRoutes()
			saved, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, input, string(saved))
			require.NoError(t, loader.Reload())
		})
	}
}
