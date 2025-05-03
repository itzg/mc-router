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

	"github.com/google/uuid"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"

	"github.com/go-kit/kit/metrics"
	"github.com/itzg/mc-router/mcproto"
	"github.com/juju/ratelimit"
	"github.com/pires/go-proxyproto"
	"github.com/sethvargo/go-retry"
	"github.com/sirupsen/logrus"
)

const (
	handshakeTimeout     = 5 * time.Second
	backendTimeout       = 30 * time.Second
	backendRetryInterval = 3 * time.Second
	backendStatusTimeout = 1 * time.Second
)

var noDeadline time.Time

type ConnectorMetrics struct {
	Errors                  metrics.Counter
	BytesTransmitted        metrics.Counter
	ConnectionsFrontend     metrics.Counter
	ConnectionsBackend      metrics.Counter
	ActiveConnections       metrics.Gauge
	ServerActivePlayer      metrics.Gauge
	ServerLogins            metrics.Counter
	ServerActiveConnections metrics.Gauge
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

type ServerMetrics struct {
	sync.RWMutex
	activeConnections map[string]int
}

func NewServerMetrics() *ServerMetrics {
	return &ServerMetrics{
		activeConnections: make(map[string]int),
	}
}

func (sm *ServerMetrics) IncrementActiveConnections(serverAddress string) {
	sm.Lock()
	defer sm.Unlock()
	if _, ok := sm.activeConnections[serverAddress]; !ok {
		sm.activeConnections[serverAddress] = 1
		return
	}
	sm.activeConnections[serverAddress] += 1
}

func (sm *ServerMetrics) DecrementActiveConnections(serverAddress string) {
	sm.Lock()
	defer sm.Unlock()
	if activeConnections, ok := sm.activeConnections[serverAddress]; ok && activeConnections <= 0 {
		sm.activeConnections[serverAddress] = 0
		return
	}
	sm.activeConnections[serverAddress] -= 1
}

func (sm *ServerMetrics) ActiveConnectionsValue(serverAddress string) int {
	sm.Lock()
	defer sm.Unlock()
	if activeConnections, ok := sm.activeConnections[serverAddress]; ok {
		return activeConnections
	}
	return 0
}

func NewConnector(metrics *ConnectorMetrics, cfg ConnectorConfig) *Connector {

	return &Connector{
		metrics:         metrics,
		connectionsCond: sync.NewCond(&sync.Mutex{}),
		config:          cfg,
		serverMetrics:   NewServerMetrics(),
		StatusCache:     NewStatusCache(),
	}
}

type ConnectorConfig struct {
	SendProxyProto             bool
	ReceiveProxyProto          bool
	TrustedProxyNets           []*net.IPNet
	RecordLogins               bool
	AutoScaleUpAllowDenyConfig *AllowDenyConfig
	AutoScaleUp                bool
	FakeOnline                 bool
	FakeOnlineMOTD             string
	CacheStatus                bool
}

type Connector struct {
	state   mcproto.State
	metrics *ConnectorMetrics

	activeConnections int32
	serverMetrics     *ServerMetrics
	connectionsCond   *sync.Cond
	ngrokToken        string
	clientFilter      *ClientFilter

	config ConnectorConfig

	StatusCache *StatusCache

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

	if c.config.ReceiveProxyProto {
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
		trustedIpNets := c.config.TrustedProxyNets

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
			playerInfo, err = c.readPlayerInfo(bufferedReader, clientAddr, handshake.NextState)
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

func (c *Connector) readPlayerInfo(bufferedReader *bufio.Reader, clientAddr net.Addr, state mcproto.State) (*PlayerInfo, error) {
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

func (c *Connector) cleanupBackendConnection(ctx context.Context, clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo, backendHostPort string, cleanupMetrics bool, checkScaleDown bool) {
	if c.connectionNotifier != nil {
		err := c.connectionNotifier.NotifyDisconnected(ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
		if err != nil {
			logrus.WithError(err).Warn("failed to notify disconnected")
		}
	}

	if cleanupMetrics {
		c.metrics.ActiveConnections.Set(float64(
			atomic.AddInt32(&c.activeConnections, -1)))

		c.serverMetrics.DecrementActiveConnections(serverAddress)
		c.metrics.ServerActiveConnections.
			With("server_address", serverAddress).
			Set(float64(c.serverMetrics.ActiveConnectionsValue(serverAddress)))

		if c.config.RecordLogins && playerInfo != nil {
			c.metrics.ServerActivePlayer.
				With("player_name", playerInfo.Name).
				With("player_uuid", playerInfo.Uuid.String()).
				With("server_address", serverAddress).
				Set(0)
		}
	}
	if checkScaleDown && c.serverMetrics.ActiveConnectionsValue(serverAddress) <= 0 {
		DownScaler.Begin(serverAddress)
	}
	c.connectionsCond.Signal()
}

func (c *Connector) findAndConnectBackend(ctx context.Context, frontendConn net.Conn,
	clientAddr net.Addr, preReadContent io.Reader, serverAddress string, playerInfo *PlayerInfo, nextState mcproto.State) {

	backendHostPort, resolvedHost, waker, _ := Routes.FindBackendForServerAddress(ctx, serverAddress)
	cleanupMetrics := false
	cleanupCheckScaleDown := false

	defer func() {
		c.cleanupBackendConnection(ctx, clientAddr, serverAddress, playerInfo, backendHostPort, cleanupMetrics, cleanupCheckScaleDown)
	}()

	if c.config.AutoScaleUp && waker != nil && nextState > mcproto.StateStatus {
		serverAllowsPlayer := c.config.AutoScaleUpAllowDenyConfig.ServerAllowsPlayer(serverAddress, playerInfo)
		logrus.
			WithField("client", clientAddr).
			WithField("server", serverAddress).
			WithField("player", playerInfo).
			WithField("serverAllowsPlayer", serverAllowsPlayer).
			Debug("checked if player is allowed to wake up the server")
		if serverAllowsPlayer {
			// Cancel down scaler if active before scale up
			DownScaler.Cancel(serverAddress)
			cleanupCheckScaleDown = true
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

	// Try to connect to the backend with a different logic depending on the state and the auto-scaling
	backendConn, err := c.retryBackendConnection(ctx, backendHostPort, nextState)

	// Failed to connect to the backend
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

		if c.connectionNotifier != nil {
			err := c.connectionNotifier.NotifyConnected(ctx, clientAddr, serverAddress, playerInfo, backendHostPort)
			if err != nil {
				logrus.WithError(err).Warn("failed to notify connected")
			}

		}

		// If the backend is offline and we are in status state, we can send a fake online status
		if nextState == mcproto.StateStatus && c.config.FakeOnline {
			c.sendFakeOnlineStatus(frontendConn, serverAddress)
		}

		return
	}

	c.metrics.ConnectionsBackend.With("host", resolvedHost).Add(1)

	c.metrics.ActiveConnections.Set(float64(
		atomic.AddInt32(&c.activeConnections, 1)))

	c.serverMetrics.IncrementActiveConnections(serverAddress)
	c.metrics.ServerActiveConnections.
		With("server_address", serverAddress).
		Set(float64(c.serverMetrics.ActiveConnectionsValue(serverAddress)))

	if c.config.RecordLogins && playerInfo != nil {
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
	if c.config.SendProxyProto {

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

func (c *Connector) retryBackendConnection(ctx context.Context, backendHostPort string, nextState mcproto.State) (net.Conn, error) {
	// We want to try to connect to the backend every backendRetryInterval
	var backendTry retry.Backoff

	// Set the retry timeouts based on the next state and autoscaler
	switch nextState {
	case mcproto.StateStatus:
		// Status request: try to connect once with backendStatusTimeout
		backendTry = retry.NewConstant(backendStatusTimeout)
		backendTry = retry.WithMaxRetries(0, backendTry)
	case mcproto.StateLogin:
		backendTry = retry.NewConstant(backendRetryInterval)
		// Connect request: if autoscaler is enabled, try to connect until backendTimeout is reached
		if c.config.AutoScaleUp {
			// Autoscaler enabled: retry until backendTimeout is reached
			backendTry = retry.WithMaxDuration(backendTimeout, backendTry)
		} else {
			// Autoscaler disabled: try to connect once with backendRetryInterval
			backendTry = retry.WithMaxRetries(0, backendTry)
		}
	default:
		// Unknown state, return error
		logrus.
			WithField("backend", backendHostPort).
			WithField("nextState", nextState).
			Error("Unknown state, unable to connect to backend")
		return nil, fmt.Errorf("unknown state: %d", nextState)
	}

	var backendConn net.Conn
	if err := retry.Do(ctx, backendTry, func(ctx context.Context) error {
		logrus.
			WithField("backend", backendHostPort).
			WithField("nextState", nextState).
			Debug("Attempting to connect to backend")

		var err error
		backendConn, err = net.Dial("tcp", backendHostPort)
		if err != nil {
			return retry.RetryableError(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return backendConn, nil
}

func (c *Connector) getFakeOnlineStatus(serverAddress string) *mcproto.StatusResponse {
	// Try to get the status from the cache
	status, hit := c.StatusCache.Get(serverAddress)
	if !hit {
		logrus.
			WithField("serverAddress", serverAddress).
			Debug("Failed to get status from cache, sending default status")

		// If we can't get the status from the cache, send a default status
		return &mcproto.StatusResponse{
			Version: mcproto.StatusVersion{
				Name:     "UNKNOWN",
				Protocol: 0,
			},
			Players: mcproto.StatusPlayers{
				Max:    0,
				Online: 0,
				Sample: []mcproto.PlayerEntry{},
			},
			Description: mcproto.StatusText{
				Text: c.config.FakeOnlineMOTD,
			},
		}
	}

	logrus.
		WithField("serverAddress", serverAddress).
		Debug("Fetched status from cache")

	// We got the status from the cache
	return status
}

func (c *Connector) sendFakeOnlineStatus(frontendConn net.Conn, serverAddress string) {
	// Get the fake online status
	status := c.getFakeOnlineStatus(serverAddress)
	// Send the status to the client
	if err := mcproto.WriteStatusResponse(frontendConn, status); err != nil {
		logrus.
			WithError(err).
			WithField("client", frontendConn.RemoteAddr()).
			WithField("status", status).
			Error("Failed to send fake online status")
		return
	}

	logrus.
		WithField("client", frontendConn.RemoteAddr()).
		WithField("status", status).
		Debug("Sent fake online status")
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
