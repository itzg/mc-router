package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookScaler_Waker(t *testing.T) {
	// A backend the waker can reach immediately so the reachability wait passes.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()

	var mu sync.Mutex
	var gotPayload WebhookScalePayload
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	scaler := NewWebhookScaler(server.URL, map[string]string{"Authorization": "Bearer secret"}, 0, 5*time.Second)

	waker := scaler.makeWakerFunc("mc.example.com", backend.Addr().String())
	require.NotNil(t, waker)

	addr, err := waker(context.Background())
	require.NoError(t, err)
	assert.Equal(t, backend.Addr().String(), addr)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, scaleActionUp, gotPayload.Action)
	assert.Equal(t, "mc.example.com", gotPayload.ServerAddress)
	assert.Equal(t, backend.Addr().String(), gotPayload.Backend)
	assert.Equal(t, "Bearer secret", gotAuth)
}

func TestWebhookScaler_WakerUsesResponseBackend(t *testing.T) {
	// The "live" backend the receiver reports; differs from the configured one.
	liveBackend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer liveBackend.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(WebhookScaleResponse{Backend: liveBackend.Addr().String()})
	}))
	defer server.Close()

	scaler := NewWebhookScaler(server.URL, nil, 0, 5*time.Second)

	// Configured backend is a dead address; the response override must win.
	waker := scaler.makeWakerFunc("mc.example.com", "10.255.255.1:25565")
	require.NotNil(t, waker)

	addr, err := waker(context.Background())
	require.NoError(t, err)
	assert.Equal(t, liveBackend.Addr().String(), addr)
}

func TestWebhookScaler_WakerFallsBackWhenResponseEmpty(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()

	// Receiver returns a non-JSON body; mc-router must ignore it and keep the
	// configured backend.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	scaler := NewWebhookScaler(server.URL, nil, 0, 5*time.Second)

	waker := scaler.makeWakerFunc("mc.example.com", backend.Addr().String())
	require.NotNil(t, waker)

	addr, err := waker(context.Background())
	require.NoError(t, err)
	assert.Equal(t, backend.Addr().String(), addr)
}

func TestParseScaleResponseBackend(t *testing.T) {
	cases := map[string]string{
		`{"backend":"10.0.0.5:25565"}`:    "10.0.0.5:25565",
		`{"backend":"  10.0.0.5:25565 "}`: "10.0.0.5:25565",
		`{"backend":""}`:                  "",
		`{}`:                              "",
		``:                                "",
		`not json`:                        "",
		`OK`:                              "",
	}
	for body, want := range cases {
		assert.Equalf(t, want, (&WebhookScaler{}).parseScaleResponseBackend(strings.NewReader(body)), "body=%q", body)
	}
}

func TestWebhookScaler_Sleeper(t *testing.T) {
	var mu sync.Mutex
	var gotPayload WebhookScalePayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	scaler := NewWebhookScaler(server.URL, nil, 0, 0)

	sleeper := scaler.makeSleeperFunc("mc.example.com", "10.0.0.5:25565")
	require.NotNil(t, sleeper)

	require.NoError(t, sleeper(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, scaleActionDown, gotPayload.Action)
	assert.Equal(t, "10.0.0.5:25565", gotPayload.Backend)
}

func TestWebhookScaler_NilFuncsWhenURLUnset(t *testing.T) {
	scaler := NewWebhookScaler("", nil, 0, 0)

	assert.Nil(t, scaler.makeWakerFunc("mc.example.com", "10.0.0.5:25565"))
	assert.Nil(t, scaler.makeSleeperFunc("mc.example.com", "10.0.0.5:25565"))
}

func TestWebhookScaler_ErrorOnNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	scaler := NewWebhookScaler(server.URL, nil, 0, 0)

	sleeper := scaler.makeSleeperFunc("mc.example.com", "10.0.0.5:25565")
	err := sleeper(context.Background())
	require.Error(t, err)
}

func TestWebhookScaler_RouteFuncsNilSafe(t *testing.T) {
	var scaler *WebhookScaler // unconfigured
	waker, sleeper := scaler.routeFuncs("mc.example.com", "10.0.0.5:25565")
	assert.Nil(t, waker)
	assert.Nil(t, sleeper)
}
