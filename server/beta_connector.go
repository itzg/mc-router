package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync/atomic"

	"github.com/pires/go-proxyproto"
	"github.com/sirupsen/logrus"

	"github.com/itzg/mc-router/mcproto"
)

func (c *Connector) HandleBetaConnection(frontendConn net.Conn, bufferedReader *bufio.Reader) {
	username, userBytes, err := mcproto.ReadBetaString(bufferedReader)
	if err != nil {
		logrus.WithError(err).Error("Failed to read username from beta handshake")
		return
	}
	logrus.WithFields(logrus.Fields{
		"user": username,
	}).Debug("Beta User detected. Initiating Version Probe.")

	// Send Fake "Offline Mode" Response to Client
	// This tricks the client into sending the Login Request (Packet 0x01)
	// Packet: 0x02 + Short(1) + UTF16("-")
	probeResponse := []byte{0x02, 0x00, 0x01, 0x00, 0x2D}
	if _, err := frontendConn.Write(probeResponse); err != nil {
		logrus.WithError(err).Error("Failed to write Beta probe response")
		return
	}

	// Read Login Request (Packet 0x01) to get Version
	// Packet: [0x01] [Int Version] [String User] ...

	id, err := bufferedReader.ReadByte()
	if err != nil || id != 0x01 {
		logrus.Error("Probe failed: Expected Login Request (0x01)")
		return
	}

	var protocolVersion int32
	if err := binary.Read(bufferedReader, binary.BigEndian, &protocolVersion); err != nil {
		logrus.WithError(err).Error("Failed to read Beta protocol version")
		return
	}

	logrus.Infof("New Beta Client: %s (Protocol %d)", username, protocolVersion)

	clientAddr := frontendConn.RemoteAddr()
	playerInfo := &PlayerInfo{
		Name: username,
		// UUID was not yet implmented for these versions
	}
	c.findAndConnectBetaBackend(frontendConn, clientAddr, bufferedReader, playerInfo, 0, false, int(protocolVersion), userBytes)
}

func (c *Connector) cleanupBackendBetaConnection(clientAddr net.Addr, protocolVersion int, playerInfo *PlayerInfo, backendHostPort string, cleanupMetrics bool, checkScaleDown bool) {
	//if c.connectionNotifier != nil {
	//	err := c.connectionNotifier.NotifyDisconnected(c.ctx, clientAddr, protocolVersion, playerInfo, backendHostPort)
	//	if err != nil {
	//		logrus.WithError(err).Warn("failed to notify disconnected")
	//	}
	//}

	if cleanupMetrics {
		c.metrics.ActiveConnections.Set(float64(
			atomic.AddInt32(&c.totalActiveConnections, -1)))

		c.activeConnections.Decrement(backendHostPort)
		c.metrics.ServerActiveConnections.
			With("protocolNumber", strconv.Itoa(protocolVersion)).
			Set(float64(c.activeConnections.GetCount(backendHostPort)))

		if c.recordLogins && playerInfo != nil {
			c.metrics.ServerActivePlayer.
				With("player_name", playerInfo.Name).
				With("player_uuid", playerInfo.Uuid.String()).
				With("protocolNumber", strconv.Itoa(protocolVersion)).
				Set(0)
		}
	}
	logrus.
		WithField("client", clientAddr).
		WithField("backendHostPort", backendHostPort).
		WithField("connectionCount", c.activeConnections.GetCount(backendHostPort)).
		Info("Closed connection to backend")
	if checkScaleDown && c.activeConnections.GetCount(backendHostPort) <= 0 {
		DownScaler.Begin(backendHostPort)
	}
	c.connectionsCond.Signal()
}

func (c *Connector) findAndConnectBetaBackend(frontendConn net.Conn,
	clientAddr net.Addr, bufferedReader *bufio.Reader, playerInfo *PlayerInfo, nextState mcproto.State, isLegacy bool, clientProtocol int, handshakeUserBytes []byte) {

	backendHostPort, resolvedHost, _, _ := Routes.FindBackendForProtocolVersion(c.ctx, clientProtocol)

	logrus.WithFields(logrus.Fields{
		"backendHostPort": backendHostPort,
		"resolvedHost":    resolvedHost,
		"clientProtocol":  clientProtocol,
		"player":          playerInfo,
	}).Info("Found beta backend:")
	cleanupMetrics := false
	cleanupCheckScaleDown := false

	defer func() {
		c.cleanupBackendBetaConnection(clientAddr, clientProtocol, playerInfo, backendHostPort, cleanupMetrics, cleanupCheckScaleDown)
	}()

	// TODO
	//if waker != nil && nextState > mcproto.StateStatus {
	//	serverAllowsPlayer := c.autoScaleUpAllowDenyConfig.ServerAllowsPlayer(serverAddress, playerInfo)
	//	logrus.
	//		WithField("client", clientAddr).
	//		WithField("protocolNumber", clientProtocol).
	//		WithField("player", playerInfo).
	//		WithField("serverAllowsPlayer", serverAllowsPlayer).
	//		Debug("checked if player is allowed to wake up the server")
	//	if serverAllowsPlayer {
	//		// Cancel down scaler if active before scale up
	//		if backendHostPort != "" {
	//			DownScaler.Cancel(backendHostPort)
	//		}
	//		cleanupCheckScaleDown = true
	//		logrus.WithField("clientProtocol", clientProtocol).Info("Waking up backend server")
	//		newBackendHostPort, err := waker(c.ctx)
	//		if err != nil {
	//			logrus.WithFields(logrus.Fields{"clientProtocol": clientProtocol}).WithError(err).Error("failed to wake up backend")
	//			c.metrics.Errors.With("type", "wakeup_failed").Add(1)
	//			return
	//		}
	//		if newBackendHostPort == "" {
	//			logrus.WithFields(logrus.Fields{"clientProtocol": clientProtocol}).Warn("waker did not return a backend address")
	//			c.metrics.Errors.With("type", "wakeup_no_address").Add(1)
	//			return
	//		}
	//		// Cancel again in case any routes were changed during wake up
	//		DownScaler.Cancel(newBackendHostPort)
	//		backendHostPort = newBackendHostPort
	//		logrus.WithFields(logrus.Fields{
	//			"clientProtocol":   clientProtocol,
	//			"backendHostPort": backendHostPort,
	//		}).Info("Woke up backend server")
	//	}
	//}

	if backendHostPort == "" {
		logrus.
			WithField("clientProtocol", clientProtocol).
			WithField("resolvedHost", resolvedHost).
			WithField("player", playerInfo).
			Warn("Unable to find registered backend")
		c.metrics.Errors.With("type", "missing_backend").Add(1)

		//if c.connectionNotifier != nil {
		//	err := c.connectionNotifier.NotifyMissingBackend(c.ctx, clientAddr, serverAddress, playerInfo)
		//	if err != nil {
		//		logrus.WithError(err).Warn("failed to notify missing backend")
		//	}
		//}

		// If status request and configured, serve predefined response
		if nextState == mcproto.StateStatus && Routes.HasRoute("beta_protocol_"+strconv.Itoa(clientProtocol)) {
			logrus.WithFields(logrus.Fields{
				"client":         clientAddr,
				"clientProtocol": clientProtocol,
				"isLegacy":       isLegacy,
			}).Debug("Missing backend: serving predefined status response")

			// Read Status Request and Ping directly from the client connection
			//br := bufio.NewReader(frontendConn)
			if isLegacy {
				c.serveLegacyStatus(frontendConn)
			} // else {
			//	c.serveStatus(frontendConn, br, serverAddress, clientProtocol)
			//}
		}
		return
	}

	logrus.
		WithField("client", clientAddr).
		WithField("clientProtocol", clientProtocol).
		WithField("backendHostPort", backendHostPort).
		WithField("player", playerInfo).
		Info("Connecting to backend")

	backendConn, err := net.Dial("tcp", backendHostPort)
	if err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			WithField("clientProtocol", clientProtocol).
			WithField("backend", backendHostPort).
			WithField("player", playerInfo).
			Warn("Unable to connect to backend")
		c.metrics.Errors.With("type", "backend_failed").Add(1)

		//if c.connectionNotifier != nil {
		//	notifyErr := c.connectionNotifier.NotifyFailedBackendConnection(c.ctx, clientAddr, serverAddress, playerInfo, backendHostPort, err)
		//	if notifyErr != nil {
		//		logrus.WithError(notifyErr).Warn("failed to notify failed backend connection")
		//	}
		//}

		return
	}

	//if c.connectionNotifier != nil {
	//	err := c.connectionNotifier.NotifyConnected(c.ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
	//	if err != nil {
	//		logrus.WithError(err).Warn("failed to notify connected")
	//	}
	//}

	// REPLAY HANDSHAKE (0x02)
	// The client sent this earlier, but we consumed it. We must send it to the now known backend.
	// Packet: [0x02] [Short Len] [Bytes...]
	handshakePacket := new(bytes.Buffer)
	handshakePacket.WriteByte(0x02)
	binary.Write(handshakePacket, binary.BigEndian, int16(len(handshakeUserBytes)/2)) // Length in chars
	handshakePacket.Write(handshakeUserBytes)

	if _, err := backendConn.Write(handshakePacket.Bytes()); err != nil {
		logrus.Error("Failed to replay handshake to backend")
		return
	}

	// We must consume the backend's actual 0x02 response so the client doesn't see it, otherwise it will crash
	backendReader := bufio.NewReader(backendConn)
	respID, err := backendReader.ReadByte()
	if err != nil {
		return
	}

	if respID == 0x02 {
		// [0x02] [Len] [Hash] - Consume and ignore
		if _, _, err := mcproto.ReadBetaString(backendReader); err != nil {
			return
		}
	} else if respID == 0xFF {
		// [0xFF] [Len] [Reason] - Kick from server
		reason, _, _ := mcproto.ReadBetaString(backendReader)
		logrus.Warnf("Backend rejected handshake: %s", reason)
		// TODO: Reconstruct kick packet for client?
		return
	}

	c.metrics.ConnectionsBackend.With("host", resolvedHost).Add(1)

	c.metrics.ActiveConnections.Set(float64(
		atomic.AddInt32(&c.totalActiveConnections, 1)))

	c.activeConnections.Increment(backendHostPort)
	//c.metrics.ServerActiveConnections.
	//	With("server_address", serverAddress).
	//	Set(float64(c.activeConnections.GetCount(backendHostPort)))

	//if c.recordLogins && playerInfo != nil {
	//	logrus.
	//		WithField("client", clientAddr).
	//		WithField("player", playerInfo).
	//		WithField("serverAddress", serverAddress).
	//		Info("Player attempted to login to server")
	//
	//	c.metrics.ServerActivePlayer.
	//		With("player_name", playerInfo.Name).
	//		With("player_uuid", playerInfo.Uuid.String()).
	//		With("server_address", serverAddress).
	//		Set(1)
	//
	//	c.metrics.ServerLogins.
	//		With("player_name", playerInfo.Name).
	//		With("player_uuid", playerInfo.Uuid.String()).
	//		With("clientProtocol", serverAddress).
	//		Add(1)
	//}

	//cleanupMetrics = true

	// PROXY protocol implementation
	if c.sendProxyProto {

		// Determine transport protocol for the PROXY header by "analyzing" the frontend connection's address
		transportProtocol := proxyproto.TCPv4
		ourHostIpPart, _, err := net.SplitHostPort(frontendConn.LocalAddr().String())
		if err != nil {
			logrus.
				WithError(err).
				WithField("localAddr", frontendConn.LocalAddr()).
				Error("Failed to extract host part of our address")
			_ = backendConn.Close()
			return
		}
		ourFrontendIp := net.ParseIP(ourHostIpPart)
		if ourFrontendIp.To4() == nil {
			transportProtocol = proxyproto.TCPv6
		}

		header := &proxyproto.Header{
			Version:           2,
			Command:           proxyproto.PROXY,
			TransportProtocol: transportProtocol,
			SourceAddr:        clientAddr,
			DestinationAddr:   frontendConn.LocalAddr(), // our end of the client's connection
		}

		_, err = header.WriteTo(backendConn)
		if err != nil {
			logrus.
				WithError(err).
				WithField("clientAddr", header.SourceAddr).
				WithField("destAddr", header.DestinationAddr).
				Error("Failed to write PROXY header")
			c.metrics.Errors.With("type", "proxy_write").Add(1)
			_ = backendConn.Close()
			return
		}
	}

	//amount, err := io.Copy(backendConn, preReadContent)
	//if err != nil {
	//	logrus.WithError(err).Error("Failed to write handshake to backend connection")
	//	c.metrics.Errors.With("type", "backend_failed").Add(1)
	//	return
	//}

	//logrus.WithField("amount", amount).Debug("Relayed handshake to backend")
	if err = frontendConn.SetReadDeadline(noDeadline); err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			Error("Failed to clear read deadline")
		c.metrics.Errors.With("type", "read_deadline").Add(1)
		return
	}

	// FORWARD LOGIN REQUEST (0x01)
	// We need to read the REST of packet 0x01 from the frontend (User, Seed, Dim)
	// We already read ID (0x01) and Protocol (Int) in HandleBetaConnection.

	// Read Username (Client sends it again in Packet 0x01)
	_, loginUserBytes, err := mcproto.ReadBetaString(bufferedReader)
	if err != nil {
		logrus.WithError(err).Error("Failed to read login username")
		return
	}

	// Read Map Seed (Long) & Dimension (Byte)
	restOfPacket := make([]byte, 9) // 8 bytes (long) + 1 byte (byte)
	if _, err := io.ReadFull(bufferedReader, restOfPacket); err != nil {
		return
	}

	// Construct the Full Login Packet for the Backend
	loginPacket := new(bytes.Buffer)
	loginPacket.WriteByte(0x01)                                               // Packet ID
	binary.Write(loginPacket, binary.BigEndian, int32(clientProtocol))        // Protocol Version
	binary.Write(loginPacket, binary.BigEndian, int16(len(loginUserBytes)/2)) // User Len
	loginPacket.Write(loginUserBytes)                                         // User Chars
	loginPacket.Write(restOfPacket)                                           // Seed + Dim

	if _, err := backendConn.Write(loginPacket.Bytes()); err != nil {
		logrus.Error("Failed to forward Login Request")
		return
	}

	c.pumpConnections(frontendConn, backendConn, playerInfo)
}
