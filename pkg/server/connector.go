package server

import (
	"bytes"
	"context"
	"io"
	"net"

	"github.com/itzg/mc-router/pkg/mcproto"
	"github.com/sirupsen/logrus"
)

type IConnector interface {
	StartAcceptingConnections(ctx context.Context, listenAddress string) error
}

var Connector IConnector = &connectorImpl{}

type connectorImpl struct {
}

func (c *connectorImpl) StartAcceptingConnections(ctx context.Context, listenAddress string) error {

	ln, err := net.Listen("tcp", listenAddress)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to start listening")
		return err
	}
	logrus.WithField("listenAddress", listenAddress).Info("Listening for Minecraft client connections")

	go c.acceptConnections(ctx, ln)

	return nil
}

func (c *connectorImpl) acceptConnections(ctx context.Context, ln net.Listener) {
	defer ln.Close()

	for {
		select {
		case <-ctx.Done():
			return

		default:
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
	defer frontendConn.Close()

	clientAddr := frontendConn.RemoteAddr()
	logrus.WithFields(logrus.Fields{"clientAddr": clientAddr}).Info("Got connection")

	inspectionBuffer := new(bytes.Buffer)

	inspectionReader := io.TeeReader(frontendConn, inspectionBuffer)

	packet, err := mcproto.ReadPacket(inspectionReader)
	if err != nil {
		logrus.WithError(err).WithField("clientAddr", clientAddr).Error("Failed to read packet")
		return
	}

	logrus.WithFields(logrus.Fields{"length": packet.Length, "packetID": packet.PacketID}).Info("Got packet")

	if packet.PacketID == mcproto.PacketIdHandshake {
		handshake, err := mcproto.ReadHandshake(packet.Data)
		if err != nil {
			logrus.WithError(err).WithField("clientAddr", clientAddr).Error("Failed to read handshake")
			return
		}

		logrus.WithFields(logrus.Fields{
			"protocolVersion": handshake.ProtocolVersion,
			"server":          handshake.ServerAddress,
			"serverPort":      handshake.ServerPort,
			"nextState":       handshake.NextState,
		}).Info("Got handshake")

		backendHostPort := Routes.FindBackendForServerAddress(handshake.ServerAddress)
		if backendHostPort == "" {
			logrus.WithField("serverAddress", handshake.ServerAddress).Warn("Unable to find registered backend")
			return
		}

		logrus.WithField("backendHostPort", backendHostPort).Info("Connecting to backend")
		backendConn, err := net.Dial("tcp", backendHostPort)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"serverAddress": handshake.ServerAddress,
				"backend":       backendHostPort,
			}).Warn("Unable to connect to backend")
			return
		}

		amount, err := io.Copy(backendConn, inspectionBuffer)
		if err != nil {
			logrus.WithError(err).Error("Failed to write handshake to backend connection")
			return
		}
		logrus.WithField("amount", amount).Debug("Relayed handshake to backend")

		pumpConnections(ctx, frontendConn, backendConn)
	} else {
		logrus.WithField("packetID", packet.PacketID).Error("Unexpected packetID, expected handshake")
	}
}

func pumpConnections(ctx context.Context, frontendConn, backendConn net.Conn) {
	defer backendConn.Close()

	errors := make(chan error, 2)

	go pumpFrames(backendConn, frontendConn, errors, "backend", "frontend")
	go pumpFrames(frontendConn, backendConn, errors, "frontend", "backend")

	for {
		select {
		case err := <-errors:
			if err != io.EOF {
				logrus.WithError(err).Error("Error observed on connection relay")
			}

			return

		case <-ctx.Done():
			return
		}
	}
}

func pumpFrames(incoming io.Reader, outgoing io.Writer, errors chan<- error, from, to string) {
	for {
		inspectionBuffer := new(bytes.Buffer)

		inspectionReader := io.TeeReader(incoming, inspectionBuffer)

		packet, err := mcproto.ReadPacket(inspectionReader)
		if err != nil {
			logrus.WithError(err).Error("Failed to read packet")
			errors <- err
			continue
		}
		amount, err := io.Copy(outgoing, inspectionBuffer)
		if err != nil {
			errors <- err
			continue
		}
		logrus.WithFields(logrus.Fields{
			"PacketID":     packet.PacketID,
			"PacketLength": packet.Length,
			"from":         from,
			"to":           to,
			"amount":       amount,
		}).Info("Proxied packet")
	}

	logrus.Infof("Finished relay %s->%s", from, to)
}
