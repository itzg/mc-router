package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"log"
	"net"
	"net/http"
	"time"
)

// WebhookNotifier implements ConnectionNotifier by sending a POST request to a webhook URL.
// The payload is a JSON object defined by WebhookNotifierPayload.
type WebhookNotifier struct {
	url         string
	requireUser bool

	client *http.Client
}

const (
	WebhookEventConnecting    = "connect"
	WebhookEventDisconnecting = "disconnect"
)

const (
	WebhookStatusMissingBackend          = "missing-backend"
	WebhookStatusFailedBackendConnection = "failed-backend-connection"
	WebhookStatusSuccess                 = "success"
)

type WebhookNotifierPayload struct {
	Event           string      `json:"event"`
	Timestamp       time.Time   `json:"timestamp"`
	Status          string      `json:"status"`
	Client          *ClientInfo `json:"client"`
	Server          string      `json:"server"`
	PlayerInfo      *PlayerInfo `json:"player,omitempty"`
	BackendHostPort string      `json:"backend,omitempty"`
	Error           string      `json:"error,omitempty"`
}

func NewWebhookNotifier(url string, requireUser bool) *WebhookNotifier {

	return &WebhookNotifier{
		url:         url,
		requireUser: requireUser,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (w *WebhookNotifier) NotifyMissingBackend(ctx context.Context, clientAddr net.Addr, server string, playerInfo *PlayerInfo) error {
	if w.requireUser && playerInfo == nil {
		return nil
	}

	payload := &WebhookNotifierPayload{
		Event:      WebhookEventConnecting,
		Timestamp:  time.Now(),
		Status:     WebhookStatusMissingBackend,
		Client:     ClientInfoFromAddr(clientAddr),
		Server:     server,
		PlayerInfo: playerInfo,
		Error:      "No backend found",
	}

	return w.send(ctx, payload)
}

func (w *WebhookNotifier) NotifyFailedBackendConnection(ctx context.Context, clientAddr net.Addr, server string,
	playerInfo *PlayerInfo, backendHostPort string, err error) error {
	if w.requireUser && playerInfo == nil {
		return nil
	}

	payload := &WebhookNotifierPayload{
		Event:           WebhookEventConnecting,
		Timestamp:       time.Now(),
		Status:          WebhookStatusFailedBackendConnection,
		Client:          ClientInfoFromAddr(clientAddr),
		Server:          server,
		PlayerInfo:      playerInfo,
		BackendHostPort: backendHostPort,
		Error:           err.Error(),
	}

	return w.send(ctx, payload)
}

func (w *WebhookNotifier) NotifyConnected(ctx context.Context, clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string) error {
	if w.requireUser && playerInfo == nil {
		return nil
	}

	payload := &WebhookNotifierPayload{
		Event:           WebhookEventConnecting,
		Timestamp:       time.Now(),
		Status:          WebhookStatusSuccess,
		Client:          ClientInfoFromAddr(clientAddr),
		Server:          serverAddress,
		PlayerInfo:      playerInfo,
		BackendHostPort: backendHostPort,
	}

	return w.send(ctx, payload)
}

func (w *WebhookNotifier) NotifyDisconnected(ctx context.Context, clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string) error {
	if w.requireUser && playerInfo == nil {
		return nil
	}

	payload := &WebhookNotifierPayload{
		Event:           WebhookEventDisconnecting,
		Timestamp:       time.Now(),
		Status:          WebhookStatusSuccess,
		Client:          ClientInfoFromAddr(clientAddr),
		Server:          serverAddress,
		PlayerInfo:      playerInfo,
		BackendHostPort: backendHostPort,
	}

	return w.send(ctx, payload)
}

func (w *WebhookNotifier) send(ctx context.Context, payload *WebhookNotifierPayload) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %v", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		w.url,
		bytes.NewBuffer(jsonPayload),
	)
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	go func() {
		resp, err := w.client.Do(req)
		if err != nil {
			// Handle error
			log.Printf("Failed to send webhook notification: %v", err)
			return
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= 400 {
			logrus.
				WithField("status", resp.StatusCode).
				Warn("webhook receiver responded with an error")
		}

	}()

	return nil
}
