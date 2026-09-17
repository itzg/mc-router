package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServerDynamicProxyProtocolConflicts(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name:   "with receive proxy protocol",
			config: Config{DynamicProxyProtocol: true, ReceiveProxyProtocol: true},
		},
		{
			name:   "with use proxy protocol",
			config: Config{DynamicProxyProtocol: true, UseProxyProtocol: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewServer(t.Context(), &test.config)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--dynamic-proxy-protocol")
		})
	}
}
