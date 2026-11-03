package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWebhookNotifier_UsesConfiguredHeaders(t *testing.T) {
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	notifier := NewWebhookNotifier(
		server.URL,
		map[string]string{
			"Authorization": "Bearer test-token",
			"X-Custom":      "custom-value",
		},
		false,
		time.Second,
		[]WebhookEvent{WebhookEventConnecting},
	)

	require.NoError(t, notifier.NotifyConnected(
		context.Background(), nil, "mc.example.com", nil, "127.0.0.1:25565",
	))

	select {
	case headers := <-received:
		require.Equal(t, "Bearer test-token", headers.Get("Authorization"))
		require.Equal(t, "custom-value", headers.Get("X-Custom"))
	case <-time.After(time.Second):
		t.Fatal("webhook request was not received")
	}
}
