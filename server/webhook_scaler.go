package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	scaleActionUp   = "up"
	scaleActionDown = "down"
)

// WebhookScalePayload is the JSON body POSTed to the scaler webhook receiver.
type WebhookScalePayload struct {
	Action        string `json:"action"`
	ServerAddress string `json:"serverAddress"`
	Backend       string `json:"backend"`
}

// WebhookScaleResponse is the optional JSON body a receiver may return from a
// scale-up call to override the configured backend for this wake — useful when
// the backend's address is dynamic (e.g. a container IP that changes on every
// start). An empty/absent body keeps the configured backend.
type WebhookScaleResponse struct {
	Backend string `json:"backend"`
}

// maxScaleResponseBody caps how much of the webhook response we read; we don't
// expect a large body here, just a small JSON backend override.
const maxScaleResponseBody = 4 << 10

// WebhookScaler scales statically-configured backends by POSTing to an external
// HTTP receiver, which owns the privilege to start/stop the backend. A single
// URL handles both directions; the payload's action field distinguishes scale
// up from scale down.
type WebhookScaler struct {
	url         string
	wakeTimeout time.Duration
	client      *webhookClient
}

// NewWebhookScaler builds a WebhookScaler. An empty url disables the
// waker/sleeper.
func NewWebhookScaler(url string, headers map[string]string, requestTimeout time.Duration, wakeTimeout time.Duration) *WebhookScaler {
	return &WebhookScaler{
		url:         url,
		wakeTimeout: wakeTimeout,
		client:      newWebhookClient(requestTimeout, headers),
	}
}

// routeFuncs returns the waker/sleeper pair for a static route. It is nil-safe
// so callers can invoke it on an unconfigured (nil) scaler.
func (s *WebhookScaler) routeFuncs(serverAddress string, backend string) (WakerFunc, SleeperFunc) {
	if s == nil {
		return nil, nil
	}
	return s.makeWakerFunc(serverAddress, backend), s.makeSleeperFunc(serverAddress, backend)
}

func (s *WebhookScaler) makeWakerFunc(serverAddress string, backend string) WakerFunc {
	if s.url == "" {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		override, err := s.send(ctx, scaleActionUp, serverAddress, backend)
		if err != nil {
			return "", fmt.Errorf("scale-up webhook failed: %w", err)
		}
		effectiveBackend := backend
		if override != "" {
			effectiveBackend = override
			logrus.WithFields(logrus.Fields{
				"serverAddress":     serverAddress,
				"configuredBackend": backend,
				"override":          override,
			}).Debug("Using backend address from scale-up response")
		}
		if err := s.waitForBackendReachable(ctx, effectiveBackend, s.wakeTimeout); err != nil {
			return effectiveBackend, err
		}
		return effectiveBackend, nil
	}
}

func (s *WebhookScaler) makeSleeperFunc(serverAddress string, backend string) SleeperFunc {
	if s.url == "" {
		return nil
	}
	return func(ctx context.Context) error {
		if _, err := s.send(ctx, scaleActionDown, serverAddress, backend); err != nil {
			return fmt.Errorf("scale-down webhook failed: %w", err)
		}
		return nil
	}
}

// send POSTs the scale request and returns an optional backend address parsed
// from the response body (empty when the receiver returns none).
func (s *WebhookScaler) send(ctx context.Context, action string, serverAddress string, backend string) (string, error) {
	logrus.WithFields(logrus.Fields{
		"action":        action,
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Calling scaler webhook")

	resp, err := s.client.postSync(ctx, s.url, &WebhookScalePayload{
		Action:        action,
		ServerAddress: serverAddress,
		Backend:       backend,
	})
	if err != nil {
		return "", err
	}
	//goland:noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	return s.parseScaleResponseBackend(resp.Body), nil
}

// parseScaleResponseBackend extracts an optional backend address from a scaler
// response body. It is lenient: an empty body, a non-JSON body, or JSON without
// a "backend" field all yield an empty string, so receivers that return nothing
// (or a plain "OK") keep working unchanged.
func (s *WebhookScaler) parseScaleResponseBackend(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxScaleResponseBody))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return ""
	}
	var parsed WebhookScaleResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		logrus.WithError(err).Debug("Ignoring unparseable scaler response body")
		return ""
	}
	return strings.TrimSpace(parsed.Backend)
}

// waitForBackendReachable blocks until a TCP connection to endpoint succeeds,
// the context is cancelled, or timeout elapses.
func (s *WebhookScaler) waitForBackendReachable(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", endpoint, 1*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for backend to become reachable at %s", endpoint)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
