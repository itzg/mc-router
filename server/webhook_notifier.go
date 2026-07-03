package server

import (
	"context"
	"net"
	"time"

	"github.com/sirupsen/logrus"
)

// WebhookNotifier implements ConnectionNotifier by sending a POST request to a webhook URL.
// The payload is a JSON object defined by WebhookNotifierPayload.
type WebhookNotifier struct {
	url         string
	requireUser bool

	client *webhookClient
	events []WebhookEvent
}

type WebhookEvent string

const (
	WebhookEventConnecting          WebhookEvent = "connect"
	WebhookEventDisconnecting       WebhookEvent = "disconnect"
	WebhookEventRouteAdded          WebhookEvent = "route-added"
	WebhookEventRouteRemoved        WebhookEvent = "route-removed"
	WebhookEventDefaultRouteSet     WebhookEvent = "default-route-set"
	WebhookEventDefaultRouteRemoved WebhookEvent = "default-route-removed"
)

type WebhookStatus string

const (
	WebhookStatusMissingBackend          WebhookStatus = "missing-backend"
	WebhookStatusFailedBackendConnection WebhookStatus = "failed-backend-connection"
	WebhookStatusSuccess                 WebhookStatus = "success"
)

type WebhookNotifierPayload struct {
	Event           WebhookEvent  `json:"event"`
	Timestamp       time.Time     `json:"timestamp"`
	Status          WebhookStatus `json:"status"`
	Client          *ClientInfo   `json:"client,omitempty"`
	Server          string        `json:"server"`
	PlayerInfo      *PlayerInfo   `json:"player,omitempty"`
	BackendHostPort string        `json:"backend,omitempty"`
	Error           string        `json:"error,omitempty"`
}

func NewWebhookNotifier(url string, requireUser bool, timeout time.Duration, events []WebhookEvent) *WebhookNotifier {
	return &WebhookNotifier{
		url:         url,
		requireUser: requireUser,
		client:      newWebhookClient(timeout, nil),
		events:      events,
	}
}

func (w *WebhookNotifier) NotifyMissingBackend(ctx context.Context, clientAddr net.Addr, server string, playerInfo *PlayerInfo) error {
	if w.requireUser && playerInfo == nil {
		return nil
	}
	if !w.allowsEvent(WebhookEventConnecting) {
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
	if !w.allowsEvent(WebhookEventConnecting) {
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
	if !w.allowsEvent(WebhookEventConnecting) {
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
	if !w.allowsEvent(WebhookEventDisconnecting) {
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

func (w *WebhookNotifier) OnRouteAdded(serverAddress string, backend string) {
	if !w.allowsEvent(WebhookEventRouteAdded) {
		return
	}
	payload := &WebhookNotifierPayload{
		Event:           WebhookEventRouteAdded,
		Timestamp:       time.Now(),
		Status:          WebhookStatusSuccess,
		Server:          serverAddress,
		BackendHostPort: backend,
	}
	err := w.send(context.Background(), payload)
	if err != nil {
		logrus.WithError(err).Error("failed to send route added webhook")
	}
}

func (w *WebhookNotifier) OnDefaultRouteSet(backend string) {
	if !w.allowsEvent(WebhookEventDefaultRouteSet) {
		return
	}
	payload := &WebhookNotifierPayload{
		Event:           WebhookEventDefaultRouteSet,
		Timestamp:       time.Now(),
		Status:          WebhookStatusSuccess,
		BackendHostPort: backend,
	}
	err := w.send(context.Background(), payload)
	if err != nil {
		logrus.WithError(err).Error("failed to send default route set webhook")
	}
}

func (w *WebhookNotifier) OnRouteRemoved(serverAddress string) {
	if !w.allowsEvent(WebhookEventRouteRemoved) {
		return
	}
	payload := &WebhookNotifierPayload{
		Event:     WebhookEventRouteRemoved,
		Timestamp: time.Now(),
		Status:    WebhookStatusSuccess,
		Server:    serverAddress,
	}
	err := w.send(context.Background(), payload)
	if err != nil {
		logrus.WithError(err).Error("failed to send route removed webhook")
	}
}

func (w *WebhookNotifier) OnDefaultRouteRemoved() {
	if !w.allowsEvent(WebhookEventDefaultRouteRemoved) {
		return
	}
	payload := &WebhookNotifierPayload{
		Event:     WebhookEventDefaultRouteRemoved,
		Timestamp: time.Now(),
		Status:    WebhookStatusSuccess,
	}
	err := w.send(context.Background(), payload)
	if err != nil {
		logrus.WithError(err).Error("failed to send default route removed webhook")
	}
}

func (w *WebhookNotifier) send(ctx context.Context, payload *WebhookNotifierPayload) error {
	return w.client.postAsync(ctx, w.url, payload)
}

func (w *WebhookNotifier) allowsEvent(event WebhookEvent) bool {
	for _, e := range w.events {
		if e == event {
			return true
		}
	}
	return false
}
