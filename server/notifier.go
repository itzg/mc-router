package server

import (
	"context"
	"net"
)

type ConnectionNotifier interface {
	NotifyMissingBackend(ctx context.Context,
		clientAddr net.Addr, serverAddress string, userInfo *UserInfo)

	NotifyFailedBackendConnection(ctx context.Context,
		clientAddr net.Addr, serverAddress string, userInfo *UserInfo, backendHostPort string, err error)

	NotifyConnected(ctx context.Context,
		clientAddr net.Addr, serverAddress string, info *UserInfo, backendHostPort string)
}
