package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"

	"github.com/go-kit/kit/metrics"
	"github.com/itzg/mc-router/mcproto"
	"github.com/juju/ratelimit"
	"github.com/pires/go-proxyproto"
	"github.com/sirupsen/logrus"
)

const (
	handshakeTimeout = 5 * time.Second
)

var noDeadline time.Time

type ConnectorMetrics struct {
	Errors              metrics.Counter
	BytesTransmitted    metrics.Counter
	ConnectionsFrontend metrics.Counter
	ConnectionsBackend  metrics.Counter
	ActiveConnections   metrics.Gauge
	ServerActivePlayer  metrics.Gauge
	ServerLogins        metrics.Counter
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

func NewConnector(metrics *ConnectorMetrics, sendProxyProto bool, receiveProxyProto bool, trustedProxyNets []*net.IPNet, recordLogins bool, autoScaleUpAllowDenyConfig *AllowDenyConfig) *Connector {
	return &Connector{
		metrics:                    metrics,
		sendProxyProto:             sendProxyProto,
		connectionsCond:            sync.NewCond(&sync.Mutex{}),
		receiveProxyProto:          receiveProxyProto,
		trustedProxyNets:           trustedProxyNets,
		recordLogins:               recordLogins,
		autoScaleUpAllowDenyConfig: autoScaleUpAllowDenyConfig,
	}
}

type Connector struct {
	state             mcproto.State
	metrics           *ConnectorMetrics
	sendProxyProto    bool
	receiveProxyProto bool
	recordLogins      bool
	trustedProxyNets  []*net.IPNet

	activeConnections          int32
	connectionsCond            *sync.Cond
	ngrokToken                 string
	clientFilter               *ClientFilter
	autoScaleUpAllowDenyConfig *AllowDenyConfig

	connectionNotifier ConnectionNotifier
}

func (c *Connector) SetConnectionNotifier(notifier ConnectionNotifier) {
	c.connectionNotifier = notifier
}

func (c *Connector) SetClientFilter(filter *ClientFilter) {
	c.clientFilter = filter
}

func (c *Connector) StartAcceptingConnections(ctx context.Context, listenAddress string, connRateLimit int) error {
	ln, err := c.createListener(ctx, listenAddress)
	if err != nil {
		return err
	}

	go c.acceptConnections(ctx, ln, connRateLimit)

	return nil
}

func (c *Connector) createListener(ctx context.Context, listenAddress string) (net.Listener, error) {
	if c.ngrokToken != "" {
		ngrokTun, err := ngrok.Listen(ctx,
			config.TCPEndpoint(),
			ngrok.WithAuthtoken(c.ngrokToken),
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
			Listener: listener,
			Policy:   c.createProxyProtoPolicy(),
		}
		logrus.Info("Using PROXY protocol listener")
		return proxyListener, nil
	}

	return listener, nil
}

func (c *Connector) createProxyProtoPolicy() func(upstream net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		trustedIpNets := c.trustedProxyNets

		if len(trustedIpNets) == 0 {
			logrus.Debug("No trusted proxy networks configured, using the PROXY header by default")
			return proxyproto.USE, nil
		}

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
		count := atomic.LoadInt32(&c.activeConnections)
		if count > 0 {
			logrus.Infof("Waiting on %d connection(s)", count)
			c.connectionsCond.Wait()
		} else {
			break
		}
	}
}

func (c *Connector) acceptConnections(ctx context.Context, ln net.Listener, connRateLimit int) {
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

func (c *Connector) HandleConnection(ctx context.Context, frontendConn net.Conn) {
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
		Info("Got connection")
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
			playerInfo, err = c.readUserInfo(bufferedReader, clientAddr, handshake.NextState)
			if err != nil {
				logrus.
					WithError(err).
					WithField("clientAddr", clientAddr).
					Error("Failed to read user info")
				c.metrics.Errors.With("type", "read").Add(1)
				return
			}
			logrus.
				WithField("client", clientAddr).
				WithField("player", playerInfo).
				Debug("Got user info")
		}

		c.findAndConnectBackend(ctx, frontendConn, clientAddr, inspectionBuffer, handshake.ServerAddress, playerInfo, handshake.NextState)

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

		c.findAndConnectBackend(ctx, frontendConn, clientAddr, inspectionBuffer, serverAddress, nil, mcproto.StateStatus)
	} else {
		logrus.
			WithField("client", clientAddr).
			WithField("packetID", packet.PacketID).
			Error("Unexpected packetID, expected handshake")
		c.metrics.Errors.With("type", "unexpected_content").Add(1)
		return
	}
}

func (c *Connector) readUserInfo(bufferedReader *bufio.Reader, clientAddr net.Addr, state mcproto.State) (*PlayerInfo, error) {
	loginPacket, err := mcproto.ReadPacket(bufferedReader, clientAddr, state)
	if err != nil {
		return nil, fmt.Errorf("failed to read login packet: %w", err)
	}

	if loginPacket.PacketID == mcproto.PacketIdLogin {
		loginStart, err := mcproto.DecodeLoginStart(loginPacket.Data)
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

func (c *Connector) findAndConnectBackend(ctx context.Context, frontendConn net.Conn,
	clientAddr net.Addr, preReadContent io.Reader, serverAddress string, playerInfo *PlayerInfo, nextState mcproto.State) {

	backendHostPort, resolvedHost, waker, _ := Routes.FindBackendForServerAddress(ctx, serverAddress)
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
			DownScaler.Cancel(serverAddress)
			// Needs to trigger after defer where c.activeConnections is decremented
			defer func() {
				activeConnections := atomic.LoadInt32(&c.activeConnections)
				if activeConnections <= 0 {
					DownScaler.Begin(serverAddress)
				}
			}()
			if err := waker(ctx); err != nil {
				logrus.WithFields(logrus.Fields{"serverAddress": serverAddress}).WithError(err).Error("failed to wake up backend")
				c.metrics.Errors.With("type", "wakeup_failed").Add(1)
				return
			}
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
			err := c.connectionNotifier.NotifyMissingBackend(ctx, clientAddr, serverAddress, playerInfo)
			if err != nil {
				logrus.WithError(err).Warn("failed to notify missing backend")
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
			notifyErr := c.connectionNotifier.NotifyFailedBackendConnection(ctx, clientAddr, serverAddress, playerInfo, backendHostPort, err)
			if notifyErr != nil {
				logrus.WithError(notifyErr).Warn("failed to notify failed backend connection")
			}
		}

		return
	}

	if c.connectionNotifier != nil {
		err := c.connectionNotifier.NotifyConnected(ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
		if err != nil {
			logrus.WithError(err).Warn("failed to notify connected")
		}
	}

	c.metrics.ConnectionsBackend.With("host", resolvedHost).Add(1)

	c.metrics.ActiveConnections.Set(float64(
		atomic.AddInt32(&c.activeConnections, 1)))
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

	defer func() {
		if c.connectionNotifier != nil {
			err := c.connectionNotifier.NotifyDisconnected(ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
			if err != nil {
				logrus.WithError(err).Warn("failed to notify disconnected")
			}
		}
		c.metrics.ActiveConnections.Set(float64(
			atomic.AddInt32(&c.activeConnections, -1)))
		if c.recordLogins && playerInfo != nil {
			c.metrics.ServerActivePlayer.
				With("player_name", playerInfo.Name).
				With("player_uuid", playerInfo.Uuid.String()).
				With("server_address", serverAddress).
				Set(0)
		}
		c.connectionsCond.Signal()
	}()

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

	c.pumpConnections(ctx, frontendConn, backendConn, playerInfo)
}

func (c *Connector) pumpConnections(ctx context.Context, frontendConn, backendConn net.Conn, playerInfo *PlayerInfo) {
	//noinspection GoUnhandledErrorResult
	defer backendConn.Close()

	clientAddr := frontendConn.RemoteAddr()
	defer logrus.WithField("client", clientAddr).Debug("Closing backend connection")

	errors := make(chan error, 2)

	go c.pumpFrames(backendConn, frontendConn, errors, "backend", "frontend", clientAddr, playerInfo)
	go c.pumpFrames(frontendConn, backendConn, errors, "frontend", "backend", clientAddr, playerInfo)

	select {
	case err := <-errors:
		if err != io.EOF {
			logrus.WithError(err).
				WithField("client", clientAddr).
				Error("Error observed on connection relay")
			c.metrics.Errors.With("type", "relay").Add(1)
		}

	case <-ctx.Done():
		logrus.Debug("Observed context cancellation")
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

func (c *Connector) UseNgrok(token string) {
	c.ngrokToken = token
}
