package server

import (
	"context"
	"net"
)

type WebhookNotifier struct {
	method string
	url    string
}

const (
	WebhookEventConnecting = "connecting"
)

type WebhookNotifierPayload struct {
	Event           string
	Status          string    `json:"status,omitempty"`
	ClientAddress   string    `json:"client-address"`
	ServerAddress   string    `json:"server-address"`
	UserInfo        *UserInfo `json:"user-info,omitempty"`
	BackendHostPort string    `json:"backend,omitempty"`
	Error           string    `json:"error,omitempty"`
}

func NewWebhookNotifier(method, url string) *WebhookNotifier {
	return &WebhookNotifier{
		method: method,
		url:    url,
	}
}

func (w *WebhookNotifier) NotifyMissingBackend(ctx context.Context, clientAddr net.Addr, serverAddress string, userInfo *UserInfo) {
	//TODO implement me
	panic("implement me")
}

func (w *WebhookNotifier) NotifyFailedBackendConnection(ctx context.Context, clientAddr net.Addr, serverAddress string, userInfo *UserInfo, backendHostPort string, err error) {
	//TODO implement me
	panic("implement me")
}

func (w *WebhookNotifier) NotifyConnected(ctx context.Context, clientAddr net.Addr, serverAddress string, info *UserInfo, backendHostPort string) {
	//TODO implement me
	panic("implement me")
}
