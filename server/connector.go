package server

import (
	"bytes"
	"context"
	"github.com/itzg/mc-router/mcproto"
	"github.com/juju/ratelimit"
	"github.com/sirupsen/logrus"
	"io"
	"net"
	"time"
)

const (
	handshakeTimeout = 5 * time.Second
)

var noDeadline time.Time

type IConnector interface {
	StartAcceptingConnections(ctx context.Context, listenAddress string, connRateLimit int) error
}

var Connector IConnector = &connectorImpl{
	state: mcproto.StateHandshaking,
}

type connectorImpl struct {
	state mcproto.State
}

func (c *connectorImpl) StartAcceptingConnections(ctx context.Context, listenAddress string, connRateLimit int) error {

	ln, err := net.Listen("tcp", listenAddress)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to start listening")
		return err
	}
	logrus.WithField("listenAddress", listenAddress).Info("Listening for Minecraft client connections")

	go c.acceptConnections(ctx, ln, connRateLimit)

	return nil
}

func (c *connectorImpl) acceptConnections(ctx context.Context, ln net.Listener, connRateLimit int) {
	//noinspection GoUnhandledErrorResult
	defer ln.Close()

	bucket := ratelimit.NewBucketWithRate(float64(connRateLimit), int64(connRateLimit*2))

	for {
		select {
		case <-ctx.Done():
			return

		case <-time.After(bucket.Take(1)):
			conn, err := ln.Accept()
			if err != nil {
				logrus.WithError(err).Error("Failed to accept connection")
			} else {
				go c.HandleConnection(ctx, conn)
			}
		}
	}
}

func (c *connectorImpl) HandleConnection(ctx context.Context, frontendConn net.Conn) {
	//noinspection GoUnhandledErrorResult
	defer frontendConn.Close()

	clientAddr := frontendConn.RemoteAddr()
	logrus.
		WithField("client", clientAddr).
		Info("Got connection")
	defer logrus.WithField("client", clientAddr).Debug("Closing frontend connection")

	inspectionBuffer := new(bytes.Buffer)

	inspectionReader := io.TeeReader(frontendConn, inspectionBuffer)

	/*	if err := frontendConn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
			logrus.
				WithError(err).
				WithField("client", clientAddr).
				Error("Failed to set read deadline")
			return
		}
	*/packet, err := mcproto.ReadPacket(inspectionReader, clientAddr, c.state)
	if err != nil {
		logrus.WithError(err).WithField("clientAddr", clientAddr).Error("Failed to read packet")
		return
	}

	logrus.
		WithField("client", clientAddr).
		WithField("length", packet.Length).
		WithField("packetID", packet.PacketID).
		Debug("Got packet")

	if packet.PacketID == mcproto.PacketIdHandshake {
		handshake, err := mcproto.ReadHandshake(packet.Data)
		if err != nil {
			logrus.WithError(err).WithField("clientAddr", clientAddr).
				Error("Failed to read handshake")
			return
		}

		logrus.
			WithField("client", clientAddr).
			WithField("handshake", handshake).
			Debug("Got handshake")

		serverAddress := handshake.ServerAddress

		c.findAndConnectBackend(ctx, frontendConn, clientAddr, inspectionBuffer, serverAddress)
	} else if packet.PacketID == mcproto.PacketIdLegacyServerListPing {
		handshake, ok := packet.Data.(*mcproto.LegacyServerListPing)
		if !ok {
			logrus.
				WithField("client", clientAddr).
				WithField("packet", packet).
				Warn("Unexpected data type for PacketIdLegacyServerListPing")
			return
		}

		logrus.
			WithField("client", clientAddr).
			WithField("handshake", handshake).
			Debug("Got legacy server list ping")

		serverAddress := handshake.ServerAddress

		c.findAndConnectBackend(ctx, frontendConn, clientAddr, inspectionBuffer, serverAddress)
	} else {
		logrus.
			WithField("client", clientAddr).
			WithField("packetID", packet.PacketID).
			Error("Unexpected packetID, expected handshake")
		return
	}
}

func (c *connectorImpl) findAndConnectBackend(ctx context.Context, frontendConn net.Conn,
	clientAddr net.Addr, preReadContent io.Reader, serverAddress string) {

	backendHostPort := Routes.FindBackendForServerAddress(serverAddress)
	if backendHostPort == "" {
		logrus.WithField("serverAddress", serverAddress).Warn("Unable to find registered backend")
		return
	}
	logrus.
		WithField("client", clientAddr).
		WithField("server", serverAddress).
		WithField("backendHostPort", backendHostPort).
		Info("Connecting to backend")
	backendConn, err := net.Dial("tcp", backendHostPort)
	if err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			WithField("serverAddress", serverAddress).
			WithField("backend", backendHostPort).
			Warn("Unable to connect to backend")
		return
	}
	amount, err := io.Copy(backendConn, preReadContent)
	if err != nil {
		logrus.WithError(err).Error("Failed to write handshake to backend connection")
		return
	}
	logrus.WithField("amount", amount).Debug("Relayed handshake to backend")
	if err = frontendConn.SetReadDeadline(noDeadline); err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			Error("Failed to clear read deadline")
		return
	}
	pumpConnections(ctx, frontendConn, backendConn)
	return
}

func pumpConnections(ctx context.Context, frontendConn, backendConn net.Conn) {
	//noinspection GoUnhandledErrorResult
	defer backendConn.Close()

	clientAddr := frontendConn.RemoteAddr()
	defer logrus.WithField("client", clientAddr).Debug("Closing backend connection")

	errors := make(chan error, 2)

	go pumpFrames(backendConn, frontendConn, errors, "backend", "frontend", clientAddr)
	go pumpFrames(frontendConn, backendConn, errors, "frontend", "backend", clientAddr)

	select {
	case err := <-errors:
		if err != io.EOF {
			logrus.WithError(err).
				WithField("client", clientAddr).
				Error("Error observed on connection relay")
		}

	case <-ctx.Done():
		logrus.Debug("Observed context cancellation")
	}
}

func pumpFrames(incoming io.Reader, outgoing io.Writer, errors chan<- error, from, to string, clientAddr net.Addr) {
	amount, err := io.Copy(outgoing, incoming)
	logrus.
		WithField("client", clientAddr).
		WithField("amount", amount).
		Infof("Finished relay %s->%s", from, to)

	if err != nil {
		errors <- err
	} else {
		// successful io.Copy return nil error, not EOF...to simulate that to trigger outer handling
		errors <- io.EOF
	}
}
