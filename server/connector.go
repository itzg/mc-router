package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"

	"github.com/itzg/mc-router/mcproto"
	"github.com/juju/ratelimit"
	"github.com/pires/go-proxyproto"
	"github.com/sirupsen/logrus"
)

const (
	handshakeTimeout = 5 * time.Second
)

var noDeadline time.Time

type ActiveConnections struct {
	sync.RWMutex
	activeConnections map[string]int
}

func NewActiveConnections() *ActiveConnections {
	return &ActiveConnections{
		activeConnections: make(map[string]int),
	}
}

func (sm *ActiveConnections) Increment(backendAddress string) {
	sm.Lock()
	defer sm.Unlock()
	if _, ok := sm.activeConnections[backendAddress]; !ok {
		sm.activeConnections[backendAddress] = 1
		return
	}
	sm.activeConnections[backendAddress] += 1
}

func (sm *ActiveConnections) Decrement(backendAddress string) {
	sm.Lock()
	defer sm.Unlock()
	if activeConnections, ok := sm.activeConnections[backendAddress]; ok && activeConnections <= 0 {
		sm.activeConnections[backendAddress] = 0
		return
	}
	sm.activeConnections[backendAddress] -= 1
}

func (sm *ActiveConnections) GetCount(backendAddress string) int {
	sm.Lock()
	defer sm.Unlock()
	if activeConnections, ok := sm.activeConnections[backendAddress]; ok {
		return activeConnections
	}
	return 0
}

func NewConnector(ctx context.Context, metrics *ConnectorMetrics, sendProxyProto bool, recordLogins bool, autoScaleUpAllowDenyConfig *AllowDenyConfig) *Connector {

	return &Connector{
		ctx:                        ctx,
		metrics:                    metrics,
		sendProxyProto:             sendProxyProto,
		connectionsCond:            sync.NewCond(&sync.Mutex{}),
		recordLogins:               recordLogins,
		autoScaleUpAllowDenyConfig: autoScaleUpAllowDenyConfig,
		activeConnections:          NewActiveConnections(),
	}
}

type NgrokConnector struct {
	token      string
	remoteAddr string
}

type Connector struct {
	ctx                        context.Context
	state                      mcproto.State
	metrics                    *ConnectorMetrics
	sendProxyProto             bool
	receiveProxyProto          bool
	recordLogins               bool
	trustedProxyNets           []*net.IPNet
	totalActiveConnections     int32
	activeConnections          *ActiveConnections
	connectionsCond            *sync.Cond
	ngrok                      NgrokConnector
	clientFilter               *ClientFilter
	autoScaleUpAllowDenyConfig *AllowDenyConfig
	connectionNotifier         ConnectionNotifier
	asleepMOTD                 string
}

func (c *Connector) UseConnectionNotifier(notifier ConnectionNotifier) {
	c.connectionNotifier = notifier
}

func (c *Connector) UseClientFilter(filter *ClientFilter) {
	c.clientFilter = filter
}

func (c *Connector) StartAcceptingConnections(listenAddress string, connRateLimit int) error {
	ln, err := c.createListener(listenAddress)
	if err != nil {
		return err
	}

	go c.acceptConnections(ln, connRateLimit)

	return nil
}

func (c *Connector) createListener(listenAddress string) (net.Listener, error) {
	if c.ngrok.token != "" {
		ngrokTun, err := ngrok.Listen(c.ctx,
			config.TCPEndpoint(
				config.WithRemoteAddr(c.ngrok.remoteAddr),
			),
			ngrok.WithAuthtoken(c.ngrok.token),
		)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start ngrok tunnel")
			return nil, err
		}
		logrus.WithField("ngrokUrl", ngrokTun.URL()).Info("Listening for Minecraft client connections via ngrok tunnel")
		return ngrokTun, nil
	}

	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to start listening")
		return nil, err
	}
	logrus.WithField("listenAddress", listenAddress).Info("Listening for Minecraft client connections")

	if c.receiveProxyProto {
		proxyListener := &proxyproto.Listener{
			Listener:   listener,
			ConnPolicy: c.createProxyProtoPolicy(),
		}
		logrus.Info("Using PROXY protocol listener")
		return proxyListener, nil
	}

	return listener, nil
}

func (c *Connector) createProxyProtoPolicy() proxyproto.ConnPolicyFunc {
	return func(connPolicyOptions proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
		trustedIpNets := c.trustedProxyNets

		if len(trustedIpNets) == 0 {
			logrus.Debug("No trusted proxy networks configured, using the PROXY header by default")
			return proxyproto.USE, nil
		}

		upstream := connPolicyOptions.Upstream
		upstreamIP := upstream.(*net.TCPAddr).IP
		for _, ipNet := range trustedIpNets {
			if ipNet.Contains(upstreamIP) {
				logrus.WithField("upstream", upstream).Debug("IP is in trusted proxies, using the PROXY header")
				return proxyproto.USE, nil
			}
		}

		logrus.WithField("upstream", upstream).Debug("IP is not in trusted proxies, discarding PROXY header")
		return proxyproto.IGNORE, nil
	}
}

func (c *Connector) WaitForConnections() {
	c.connectionsCond.L.Lock()
	defer c.connectionsCond.L.Unlock()

	for {
		count := atomic.LoadInt32(&c.totalActiveConnections)
		if count > 0 {
			logrus.Infof("Waiting on %d connection(s)", count)
			c.connectionsCond.Wait()
		} else {
			return
		}
	}
}

// AcceptConnection provides a way to externally supply a connection to consume.
// Note that this will skip rate limiting.
func (c *Connector) AcceptConnection(conn net.Conn) {
	go c.HandleConnection(conn)
}

func (c *Connector) acceptConnections(ln net.Listener, connRateLimit int) {
	//noinspection GoUnhandledErrorResult
	defer ln.Close()

	bucket := ratelimit.NewBucketWithRate(float64(connRateLimit), int64(connRateLimit*2))

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-time.After(bucket.Take(1)):
			conn, err := ln.Accept()
			if err != nil {
				logrus.WithError(err).Error("Failed to accept connection")
			} else {
				go c.HandleConnection(conn)
			}
		}
	}
}

func (c *Connector) HandleConnection(frontendConn net.Conn) {
	c.metrics.ConnectionsFrontend.Add(1)
	//noinspection GoUnhandledErrorResult
	defer frontendConn.Close()

	clientAddr := frontendConn.RemoteAddr()

	if tcpAddr, ok := clientAddr.(*net.TCPAddr); ok {
		if c.clientFilter != nil {
			allow := c.clientFilter.Allow(tcpAddr.AddrPort())
			if !allow {
				logrus.WithField("client", clientAddr).Debug("Client is blocked")
				return
			}
		}
	} else {
		logrus.WithField("client", clientAddr).Warn("Remote address is not a TCP address, skipping filtering")
	}

	logrus.
		WithField("client", clientAddr).
		Debug("Got connection")
	defer logrus.WithField("client", clientAddr).Debug("Closing frontend connection")

	// Tee-off the inspected content to a buffer so that we can retransmit it to the backend connection
	inspectionBuffer := new(bytes.Buffer)
	inspectionReader := io.TeeReader(frontendConn, inspectionBuffer)

	bufferedReader := bufio.NewReader(inspectionReader)

	if err := frontendConn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			Error("Failed to set read deadline")
		c.metrics.Errors.With("type", "read_deadline").Add(1)
		return
	}
	packet, err := mcproto.ReadPacket(bufferedReader, clientAddr, c.state)
	if err != nil {
		logrus.WithError(err).WithField("clientAddr", clientAddr).Error("Failed to read packet")
		c.metrics.Errors.With("type", "read").Add(1)
		return
	}

	logrus.
		WithField("client", clientAddr).
		WithField("length", packet.Length).
		WithField("packetID", packet.PacketID).
		Debug("Got packet")

	if packet.PacketID == mcproto.PacketIdHandshake {
		handshake, err := mcproto.DecodeHandshake(packet.Data)
		if err != nil {
			logrus.WithError(err).WithField("clientAddr", clientAddr).
				Error("Failed to read handshake")
			c.metrics.Errors.With("type", "read").Add(1)
			return
		}

		logrus.
			WithField("client", clientAddr).
			WithField("handshake", handshake).
			Debug("Got handshake")

		var playerInfo *PlayerInfo = nil
		if handshake.NextState == mcproto.StateLogin {
			playerInfo, err = c.readPlayerInfo(handshake.ProtocolVersion, bufferedReader, clientAddr, handshake.NextState)
			if err != nil {
				if errors.Is(err, io.EOF) {
					logrus.
						WithError(err).
						WithField("clientAddr", clientAddr).
						WithField("player", playerInfo).
						Warn("Truncated buffer while reading player info")
				} else {
					logrus.
						WithError(err).
						WithField("clientAddr", clientAddr).
						Error("Failed to read user info")
					c.metrics.Errors.With("type", "read").Add(1)
					return
				}
			}
			logrus.
				WithField("client", clientAddr).
				WithField("player", playerInfo).
				Debug("Got user info")
		}

		c.findAndConnectBackend(frontendConn, clientAddr, inspectionBuffer, handshake.ServerAddress, playerInfo, handshake.NextState, false, int(handshake.ProtocolVersion))

	} else if packet.PacketID == mcproto.PacketIdLegacyServerListPing {
		handshake, ok := packet.Data.(*mcproto.LegacyServerListPing)
		if !ok {
			logrus.
				WithField("client", clientAddr).
				WithField("packet", packet).
				Warn("Unexpected data type for PacketIdLegacyServerListPing")
			c.metrics.Errors.With("type", "unexpected_content").Add(1)
			return
		}

		logrus.
			WithField("client", clientAddr).
			WithField("handshake", handshake).
			Debug("Got legacy server list ping")

		serverAddress := handshake.ServerAddress

		c.findAndConnectBackend(frontendConn, clientAddr, inspectionBuffer, serverAddress, nil, mcproto.StateStatus, true, 0)
	} else {
		logrus.
			WithField("client", clientAddr).
			WithField("packetID", packet.PacketID).
			Error("Unexpected packetID, expected handshake")
		c.metrics.Errors.With("type", "unexpected_content").Add(1)
		return
	}
}

// serveStatus writes a predefined status JSON and optionally handles ping/pong
func (c *Connector) serveStatus(frontendConn net.Conn, reader *bufio.Reader, serverAddress string, clientProtocol int) {
	motd := Routes.GetAsleepMOTD(serverAddress)
	if motd == "" {
		motd = c.asleepMOTD
	}
	if motd == "" {
		return
	}

	// Consume Status Request (0x00) if present; some clients may send Ping (0x01) directly
	_ = frontendConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	firstPkt, err := mcproto.ReadPacket(reader, frontendConn.RemoteAddr(), mcproto.StateStatus)
	var pingPending bool
	var pingVal int64
	if err == nil && firstPkt != nil {
		if firstPkt.PacketID == mcproto.PacketIdPingRequest {
			if payload, ok := firstPkt.Data.(mcproto.PingPayload); ok {
				pingPending = true
				pingVal = payload.Timestamp
				logrus.WithFields(logrus.Fields{
					"client":   frontendConn.RemoteAddr(),
					"ping_val": pingVal,
				}).Debug("Predefined status: received immediate ping")
			}
		}
		// else 0x00 is the normal status request; proceed to write response
	} else if err != nil {
		logrus.WithFields(logrus.Fields{
			"client": frontendConn.RemoteAddr(),
			"error":  err,
		}).Warn("Predefined status: error reading initial status packet")
	}

	// Build and write Status Response
	viName, viProto := c.getVersionInfo(serverAddress, clientProtocol)
	var status mcproto.StatusResponse
	status.Version.Name = viName
	status.Version.Protocol = viProto
	status.Players.Max = 1
	status.Players.Online = 0
	status.Description = map[string]interface{}{"text": motd}

	// Write Status Response
	_ = frontendConn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if err := mcproto.WriteStatusFromStruct(frontendConn, status); err != nil {
		logrus.WithError(err).Warn("Failed to write predefined status response")
		return
	}

	// If we didn't already get a ping, briefly wait for one
	if !pingPending {
		_ = frontendConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if nextPkt, err2 := mcproto.ReadPacket(reader, frontendConn.RemoteAddr(), mcproto.StateStatus); err2 == nil && nextPkt != nil {
			if nextPkt.PacketID == mcproto.PacketIdPingRequest {
				if payload, ok := nextPkt.Data.(mcproto.PingPayload); ok {
					pingPending = true
					pingVal = payload.Timestamp
					logrus.WithFields(logrus.Fields{
						"client":   frontendConn.RemoteAddr(),
						"ping_val": pingVal,
					}).Debug("Predefined status: received ping after status")
				}
			}
		} else if err2 != nil {
			logrus.WithFields(logrus.Fields{
				"client": frontendConn.RemoteAddr(),
				"error":  err2,
			}).Debug("Predefined status: error/timeout reading ping after status")
		}
	}
	if pingPending {
		if err := mcproto.WritePongPacket(frontendConn, pingVal); err != nil {
			logrus.WithFields(logrus.Fields{
				"client": frontendConn.RemoteAddr(),
				"error":  err,
			}).Warn("Predefined status: failed to write pong")
		} else {
			logrus.WithFields(logrus.Fields{
				"client":   frontendConn.RemoteAddr(),
				"ping_val": pingVal,
			}).Debug("Predefined status: wrote pong")
		}
	} else {
		logrus.WithFields(logrus.Fields{
			"client": frontendConn.RemoteAddr(),
		}).Debug("Predefined status: no ping received, closing")
	}
}

// serveLegacyStatus writes a simple legacy SLP response and closes the connection
func (c *Connector) serveLegacyStatus(frontendConn net.Conn) {
	motd := c.asleepMOTD
	if motd == "" {
		return
	}
	_ = frontendConn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	// 127 protocol for legacy response per spec; version name and motd from predefined JSON if available
	// write a basic response: protocol=127, version="1.7+", motd, online=0, max=1
	if err := mcproto.WriteLegacySLPResponse(frontendConn, 127, "1.7+", motd, 0, 1); err != nil {
		logrus.WithError(err).Warn("Failed to write legacy SLP response")
	}
}

func (c *Connector) readPlayerInfo(protocolVersion mcproto.ProtocolVersion, bufferedReader *bufio.Reader, clientAddr net.Addr, state mcproto.State) (*PlayerInfo, error) {
	loginPacket, err := mcproto.ReadPacket(bufferedReader, clientAddr, state)
	if err != nil {
		return nil, fmt.Errorf("failed to read login packet: %w", err)
	}

	if loginPacket.PacketID == mcproto.PacketIdLogin {
		loginStart, err := mcproto.DecodeLoginStart(protocolVersion, loginPacket.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode login start: %w", err)
		}
		return &PlayerInfo{
			Name: loginStart.Name,
			Uuid: loginStart.PlayerUuid,
		}, nil
	} else {
		return nil, fmt.Errorf("expected login packet, got %d", loginPacket.PacketID)
	}
}

func (c *Connector) cleanupBackendConnection(clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string, cleanupMetrics bool, checkScaleDown bool) {
	if c.connectionNotifier != nil {
		err := c.connectionNotifier.NotifyDisconnected(c.ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
		if err != nil {
			logrus.WithError(err).Warn("failed to notify disconnected")
		}
	}

	if cleanupMetrics {
		c.metrics.ActiveConnections.Set(float64(
			atomic.AddInt32(&c.totalActiveConnections, -1)))

		c.activeConnections.Decrement(backendHostPort)
		c.metrics.ServerActiveConnections.
			With("server_address", serverAddress).
			Set(float64(c.activeConnections.GetCount(backendHostPort)))

		if c.recordLogins && playerInfo != nil {
			c.metrics.ServerActivePlayer.
				With("player_name", playerInfo.Name).
				With("player_uuid", playerInfo.Uuid.String()).
				With("server_address", serverAddress).
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

func (c *Connector) findAndConnectBackend(frontendConn net.Conn,
	clientAddr net.Addr, preReadContent io.Reader, serverAddress string, playerInfo *PlayerInfo, nextState mcproto.State, isLegacy bool, clientProtocol int) {

	backendHostPort, resolvedHost, waker, _ := Routes.FindBackendForServerAddress(c.ctx, serverAddress)
	cleanupMetrics := false
	cleanupCheckScaleDown := false

	defer func() {
		c.cleanupBackendConnection(clientAddr, serverAddress, playerInfo, backendHostPort, cleanupMetrics, cleanupCheckScaleDown)
	}()

	if waker != nil && nextState > mcproto.StateStatus {
		serverAllowsPlayer := c.autoScaleUpAllowDenyConfig.ServerAllowsPlayer(serverAddress, playerInfo)
		logrus.
			WithField("client", clientAddr).
			WithField("server", serverAddress).
			WithField("player", playerInfo).
			WithField("serverAllowsPlayer", serverAllowsPlayer).
			Debug("checked if player is allowed to wake up the server")
		if serverAllowsPlayer {
			// Cancel down scaler if active before scale up
			if backendHostPort != "" {
				DownScaler.Cancel(backendHostPort)
			}
			cleanupCheckScaleDown = true
			logrus.WithField("serverAddress", serverAddress).Info("Waking up backend server")
			newBackendHostPort, err := waker(c.ctx)
			if err != nil {
				logrus.WithFields(logrus.Fields{"serverAddress": serverAddress}).WithError(err).Error("failed to wake up backend")
				c.metrics.Errors.With("type", "wakeup_failed").Add(1)
				return
			}
			if newBackendHostPort == "" {
				logrus.WithFields(logrus.Fields{"serverAddress": serverAddress}).Warn("waker did not return a backend address")
				c.metrics.Errors.With("type", "wakeup_no_address").Add(1)
				return
			}
			// Cancel again in case any routes were changed during wake up
			DownScaler.Cancel(newBackendHostPort)
			backendHostPort = newBackendHostPort
			logrus.WithFields(logrus.Fields{
				"serverAddress":   serverAddress,
				"backendHostPort": backendHostPort,
			}).Info("Woke up backend server")
		}
	}

	if backendHostPort == "" {
		logrus.
			WithField("serverAddress", serverAddress).
			WithField("resolvedHost", resolvedHost).
			WithField("player", playerInfo).
			Warn("Unable to find registered backend")
		c.metrics.Errors.With("type", "missing_backend").Add(1)

		if c.connectionNotifier != nil {
			err := c.connectionNotifier.NotifyMissingBackend(c.ctx, clientAddr, serverAddress, playerInfo)
			if err != nil {
				logrus.WithError(err).Warn("failed to notify missing backend")
			}
		}

		// If status request and configured, serve predefined response
		if nextState == mcproto.StateStatus && Routes.HasRoute(serverAddress) {
			logrus.WithFields(logrus.Fields{
				"client":   clientAddr,
				"server":   serverAddress,
				"isLegacy": isLegacy,
			}).Debug("Missing backend: serving predefined status response")

			// Read Status Request and Ping directly from the client connection
			br := bufio.NewReader(frontendConn)
			if isLegacy {
				c.serveLegacyStatus(frontendConn)
			} else {
				c.serveStatus(frontendConn, br, serverAddress, clientProtocol)
			}
		}
		return
	}

	logrus.
		WithField("client", clientAddr).
		WithField("server", serverAddress).
		WithField("backendHostPort", backendHostPort).
		WithField("player", playerInfo).
		Info("Connecting to backend")

	backendConn, err := net.Dial("tcp", backendHostPort)
	if err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			WithField("serverAddress", serverAddress).
			WithField("backend", backendHostPort).
			WithField("player", playerInfo).
			Warn("Unable to connect to backend")
		c.metrics.Errors.With("type", "backend_failed").Add(1)

		if c.connectionNotifier != nil {
			notifyErr := c.connectionNotifier.NotifyFailedBackendConnection(c.ctx, clientAddr, serverAddress, playerInfo, backendHostPort, err)
			if notifyErr != nil {
				logrus.WithError(notifyErr).Warn("failed to notify failed backend connection")
			}
		}

		return
	}

	if c.connectionNotifier != nil {
		err := c.connectionNotifier.NotifyConnected(c.ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
		if err != nil {
			logrus.WithError(err).Warn("failed to notify connected")
		}
	}

	c.metrics.ConnectionsBackend.With("host", resolvedHost).Add(1)

	c.metrics.ActiveConnections.Set(float64(
		atomic.AddInt32(&c.totalActiveConnections, 1)))

	c.activeConnections.Increment(backendHostPort)
	c.metrics.ServerActiveConnections.
		With("server_address", serverAddress).
		Set(float64(c.activeConnections.GetCount(backendHostPort)))

	if c.recordLogins && playerInfo != nil {
		logrus.
			WithField("client", clientAddr).
			WithField("player", playerInfo).
			WithField("serverAddress", serverAddress).
			Info("Player attempted to login to server")

		c.metrics.ServerActivePlayer.
			With("player_name", playerInfo.Name).
			With("player_uuid", playerInfo.Uuid.String()).
			With("server_address", serverAddress).
			Set(1)

		c.metrics.ServerLogins.
			With("player_name", playerInfo.Name).
			With("player_uuid", playerInfo.Uuid.String()).
			With("server_address", serverAddress).
			Add(1)
	}

	cleanupMetrics = true

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

	amount, err := io.Copy(backendConn, preReadContent)
	if err != nil {
		logrus.WithError(err).Error("Failed to write handshake to backend connection")
		c.metrics.Errors.With("type", "backend_failed").Add(1)
		return
	}

	logrus.WithField("amount", amount).Debug("Relayed handshake to backend")
	if err = frontendConn.SetReadDeadline(noDeadline); err != nil {
		logrus.
			WithError(err).
			WithField("client", clientAddr).
			Error("Failed to clear read deadline")
		c.metrics.Errors.With("type", "read_deadline").Add(1)
		return
	}

	c.pumpConnections(frontendConn, backendConn, playerInfo)
}

func (c *Connector) pumpConnections(frontendConn, backendConn net.Conn, playerInfo *PlayerInfo) {
	//noinspection GoUnhandledErrorResult
	defer backendConn.Close()

	clientAddr := frontendConn.RemoteAddr()
	defer logrus.WithField("client", clientAddr).Debug("Closing backend connection")

	errorsChan := make(chan error, 2)

	go c.pumpFrames(backendConn, frontendConn, errorsChan, "backend", "frontend", clientAddr, playerInfo)
	go c.pumpFrames(frontendConn, backendConn, errorsChan, "frontend", "backend", clientAddr, playerInfo)

	select {
	case err := <-errorsChan:
		if err != io.EOF {
			logrus.WithError(err).
				WithField("client", clientAddr).
				Error("Error observed on connection relay")
			c.metrics.Errors.With("type", "relay").Add(1)
		}

	case <-c.ctx.Done():
		logrus.Debug("Connector observed context cancellation")
	}
}

func (c *Connector) pumpFrames(incoming io.Reader, outgoing io.Writer, errors chan<- error, from, to string,
	clientAddr net.Addr, playerInfo *PlayerInfo) {
	amount, err := io.Copy(outgoing, incoming)
	logrus.
		WithField("client", clientAddr).
		WithField("amount", amount).
		WithField("player", playerInfo).
		Infof("Finished relay %s->%s", from, to)

	c.metrics.BytesTransmitted.Add(float64(amount))

	if err != nil {
		errors <- err
	} else {
		// successful io.Copy return nil error, not EOF...to simulate that to trigger outer handling
		errors <- io.EOF
	}
}

func (c *Connector) UseNgrok(config NgrokConfig) {
	c.ngrok.token = config.Token
	c.ngrok.remoteAddr = config.RemoteAddr
}

func (c *Connector) UseReceiveProxyProto(trustedProxyNets []*net.IPNet) {
	c.trustedProxyNets = trustedProxyNets
	c.receiveProxyProto = true
}

// UseAsleepMOTD configures a predefined MOTD to serve when backends are asleep
func (c *Connector) UseAsleepMOTD(motd string) {
	c.asleepMOTD = motd
}

// getVersionInfo falls back to client protocol and a derived name but in future
// could be extended to cache server-reported versions
func (c *Connector) getVersionInfo(_ string, clientProtocol int) (string, int) {
	// no cache; use client protocol
	return protocolToName(clientProtocol), clientProtocol
}

// protocolToName maps protocol numbers to a friendly name; falls back to "1.7+"
func protocolToName(proto int) string {
	switch proto {
	// TODO: expand this mapping as needed
	default:
		return "1.7+"
	}
}
