package server

import (
	"context"
	"net"
)

type ConnectionNotifier interface {
	// NotifyMissingBackend is called when an inbound connection is received for a server that does not have a backend.
	NotifyMissingBackend(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo) error

	// NotifyFailedBackendConnection is called when the backend connection failed.
	NotifyFailedBackendConnection(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string, err error) error

	// NotifyConnected is called when the backend connection succeeded.
	NotifyConnected(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string) error
}
