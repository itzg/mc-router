package server

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net"
)

type PlayerInfo struct {
	Name string    `json:"name"`
	Uuid uuid.UUID `json:"uuid"`
}

func (p *PlayerInfo) String() string {
	if p == nil {
		return ""
	}

	return fmt.Sprintf("%s/%s", p.Name, p.Uuid)
}

type ClientInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func ClientInfoFromAddr(addr net.Addr) *ClientInfo {
	if addr == nil {
		return nil
	}

	return &ClientInfo{
		Host: addr.(*net.TCPAddr).IP.String(),
		Port: addr.(*net.TCPAddr).Port,
	}
}

type ConnectionNotifier interface {
	// NotifyMissingBackend is called when an inbound connection is received for a server that does not have a backend.
	NotifyMissingBackend(ctx context.Context, clientAddr net.Addr, server string, playerInfo *PlayerInfo) error

	// NotifyFailedBackendConnection is called when the backend connection failed.
	NotifyFailedBackendConnection(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string, err error) error

	// NotifyConnected is called when the backend connection succeeded.
	NotifyConnected(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string) error

	// NotifyDisconnected is called when the backend connection terminates.
	NotifyDisconnected(ctx context.Context,
		clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string) error
}
