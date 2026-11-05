package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteConfigJSON(t *testing.T) {
	for _, tt := range []struct {
		name, input, output string
		override            *bool
		invalid             bool
	}{
		{name: "string", input: `"backend:25565"`, output: `"backend:25565"`},
		{name: "object", input: `{"backend":"backend:25565"}`, output: `"backend:25565"`},
		{name: "true", input: `{"backend":"backend:25565","proxy-protocol":true}`, output: `{"backend":"backend:25565","proxy-protocol":true}`, override: new(true)},
		{name: "false", input: `{"backend":"backend:25565","proxy-protocol":false}`, output: `{"backend":"backend:25565","proxy-protocol":false}`, override: new(false)},
		{name: "null override", input: `{"backend":"backend:25565","proxy-protocol":null}`, output: `"backend:25565"`},
		{name: "null route", input: `null`, output: `""`},
		{name: "number", input: `42`, invalid: true},
		{name: "boolean", input: `false`, invalid: true},
		{name: "array", input: `[]`, invalid: true},
		{name: "invalid backend", input: `{"backend":42}`, invalid: true},
		{name: "invalid override", input: `{"proxy-protocol":"false"}`, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Reused destinations must clear previous fields for both JSON forms.
			config := RouteConfig{Backend: "stale", ProxyProtocol: new(true)}
			err := json.Unmarshal([]byte(tt.input), &config)
			if tt.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.override, config.ProxyProtocol)
			encoded, err := json.Marshal(config)
			require.NoError(t, err)
			assert.JSONEq(t, tt.output, string(encoded))
		})
	}
}

func TestRouteProxyProtocolLookup(t *testing.T) {
	routes := NewRoutes(t.Context())
	routes.SimplifySRV(true)
	routes.SetDefaultRoute("default:25565", nil, RouteOptions{ProxyProtocol: new(true)})
	routes.CreateMapping("mc.example.com", "backend:25565", nil, RouteOptions{ProxyProtocol: new(false)})
	routes.CreateMapping("inherit.example.com", "backend:25565", nil, RouteOptions{})
	for _, tt := range []struct {
		host string
		want *bool
	}{
		{"mc.example.com", new(false)}, {"MC.EXAMPLE.COM.", new(false)},
		{"mc.example.com///shield", new(false)}, {"_minecraft._tcp.mc.example.com", new(false)},
		{"inherit.example.com", nil}, {"unknown.example.com", new(true)},
	} {
		t.Run(tt.host, func(t *testing.T) { assert.Equal(t, tt.want, routes.GetProxyProtocol(tt.host)) })
	}
	routes.UpdateMapping("mc.example.com", "updated:25565", nil, RouteOptions{})
	assert.Nil(t, routes.GetProxyProtocol("mc.example.com"))
	routes.CreateMapping("mc.example.com", "updated:25565", nil, RouteOptions{ProxyProtocol: new(true)})
	routes.CreateMapping("mc.example.com", "updated:25565", nil, RouteOptions{})
	assert.Nil(t, routes.GetProxyProtocol("mc.example.com"))
	routes.SetDefaultRoute("default:25565", nil, RouteOptions{})
	assert.Nil(t, routes.GetProxyProtocol("unknown.example.com"))
	assert.Nil(t, NewRoutes(t.Context()).GetProxyProtocol("unknown"))
}
