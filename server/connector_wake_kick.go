package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/sirupsen/logrus"
)

const (
	// wakeHoldChannel names the login plugin requests that keep a held login alive. No client
	// knows it, so each request is answered as not understood.
	wakeHoldChannel = "mc-router:wait"
	// defaultWakeHoldInterval is well within the 30 seconds the Java client waits for any
	// packet before it gives up on a connection.
	defaultWakeHoldInterval = 10 * time.Second
	// wakeHoldDrainTimeout bounds the wait for outstanding responses once the backend is ready
	wakeHoldDrainTimeout = 5 * time.Second
)

var errUnexpectedWakeHoldContent = errors.New("unexpected content from held client")

// wakeSignal announces the end of a background wake to the logins held for it
type wakeSignal struct {
	done chan struct{}
	// backendHostPort and err are the waker's result, valid once done is closed
	backendHostPort string
	err             error
}

func (c *Connector) kickWhileWaking(frontendConn net.Conn, clientAddr net.Addr, serverAddress string, playerInfo *PlayerInfo) {
	_ = frontendConn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if err := mcproto.WriteLoginDisconnect(frontendConn, c.wakeKickMessage); err != nil {
		logrus.WithError(err).
			WithField("client", clientAddr).
			Warn("Failed to write wake-up disconnect message")
		return
	}
	logrus.WithFields(logrus.Fields{
		"client":        clientAddr,
		"serverAddress": serverAddress,
		"player":        playerInfo,
	}).Info("Disconnected player with wake-up message while backend starts")
}

// holdWhileWaking keeps a login waiting for the background wake of its backend. The backend
// sends nothing during that time, so a login plugin request per interval resets the client's
// read timeout, and the client's responses are consumed here. Returns the backend to relay the
// login to, with preReadContent holding what the client sent before the hold, or an empty
// string when the client was disconnected instead. Giving up on a client never stops the wake.
func (c *Connector) holdWhileWaking(frontendConn net.Conn, clientAddr net.Addr, frontendReader *bufio.Reader,
	preReadContent *bytes.Buffer, serverAddress string, playerInfo *PlayerInfo) string {

	logger := logrus.WithFields(logrus.Fields{
		"client":        clientAddr,
		"serverAddress": serverAddress,
		"player":        playerInfo,
	})

	signal, waking := c.wakeSignals.Load(serverAddress)
	if !waking {
		logger.Debug("Not holding login: no background wake of this server address to wait for")
		c.kickWhileWaking(frontendConn, clientAddr, serverAddress, playerInfo)
		return ""
	}
	// A client sends nothing after its login start until the server responds
	if frontendReader.Buffered() > 0 {
		logger.Debug("Not holding login: client sent more than its login start")
		c.kickWhileWaking(frontendConn, clientAddr, serverAddress, playerInfo)
		return ""
	}
	wake := signal.(*wakeSignal)
	preReadLen := preReadContent.Len()

	logger.Info("Holding login while backend starts")
	c.metrics.IncrementWakeHolds(wakeHoldStarted)

	// Responses arrive at the pace of the requests rather than within the handshake timeout
	_ = frontendConn.SetReadDeadline(noDeadline)
	responses := make(chan int32)
	readerDone := make(chan error, 1)
	stopped := make(chan struct{})
	stopReader := sync.OnceFunc(func() { close(stopped) })
	defer stopReader()
	go func() {
		readerDone <- readWakeHoldResponses(frontendReader, clientAddr, responses, stopped)
	}()

	var nextMessageID int32
	outstanding := make(map[int32]struct{})
	requestResponse := func() error {
		_ = frontendConn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		if err := mcproto.WriteLoginPluginRequest(frontendConn, nextMessageID, wakeHoldChannel, nil); err != nil {
			return err
		}
		outstanding[nextMessageID] = struct{}{}
		nextMessageID++
		return nil
	}
	if err := requestResponse(); err != nil {
		logger.WithError(err).Debug("Failed to write to held login")
		return ""
	}

	ticker := time.NewTicker(c.wakeHoldInterval)
	defer ticker.Stop()
	expired := time.NewTimer(c.wakeHold)
	defer expired.Stop()
	keepAlive, ready := ticker.C, wake.done
	var drained <-chan time.Time

	// The backend must never see a response to one of our requests, so even when it is ready
	// the login is only relayed after every request got its response
	for ready != nil || len(outstanding) > 0 {
		select {
		case <-keepAlive:
			if len(outstanding) > 1 {
				logger.Info("Held login stopped responding")
				return ""
			}
			if err := requestResponse(); err != nil {
				logger.WithError(err).Debug("Failed to write to held login")
				return ""
			}

		case messageID := <-responses:
			if _, ok := outstanding[messageID]; !ok {
				logger.WithField("messageID", messageID).Warn("Unexpected login plugin response from held login")
				c.metrics.IncrementErrors("unexpected_content")
				return ""
			}
			delete(outstanding, messageID)

		case err := <-readerDone:
			if errors.Is(err, errUnexpectedWakeHoldContent) {
				logger.WithError(err).Warn("Unexpected content from held login")
				c.metrics.IncrementErrors("unexpected_content")
			} else {
				logger.WithError(err).Info("Held login left while backend starts")
				c.metrics.IncrementWakeHolds(wakeHoldGaveUp)
			}
			return ""

		case <-ready:
			if wake.err != nil || wake.backendHostPort == "" || !c.backendAcceptsConnections(wake.backendHostPort) {
				// the wake failed or timed out, which has been reported by wakeInBackground
				logger.WithError(wake.err).Debug("Wake ended without a reachable backend for held login")
				c.kickWhileWaking(frontendConn, clientAddr, serverAddress, playerInfo)
				return ""
			}
			keepAlive, ready = nil, nil
			drained = time.After(wakeHoldDrainTimeout)

		case <-drained:
			logger.Info("Held login did not respond in time to be connected")
			return ""

		case <-expired.C:
			logger.WithField("wakeHold", c.wakeHold).Info("Backend was not ready in time for held login")
			c.metrics.IncrementWakeHolds(wakeHoldExpired)
			c.kickWhileWaking(frontendConn, clientAddr, serverAddress, playerInfo)
			return ""

		case <-c.ctx.Done():
			logger.Debug("Shutting down with login still held")
			return ""
		}
	}

	// Stop the reader, which waits for a packet that will not come, by expiring its read
	stopReader()
	if err := frontendConn.SetReadDeadline(time.Now()); err != nil {
		logger.WithError(err).Error("Failed to set read deadline")
		c.metrics.IncrementErrors("read_deadline")
		return ""
	}
	err := <-readerDone
	if !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, errUnexpectedWakeHoldContent) || frontendReader.Buffered() > 0 {
		logger.WithError(err).Warn("Unexpected content from held login")
		c.metrics.IncrementErrors("unexpected_content")
		return ""
	}
	if err := frontendConn.SetWriteDeadline(noDeadline); err != nil {
		logger.WithError(err).Error("Failed to clear write deadline")
		return ""
	}
	// Drop the responses, so that the backend gets the handshake and login start only
	preReadContent.Truncate(preReadLen)

	logger.WithField("backendHostPort", wake.backendHostPort).Info("Backend is ready for held login")
	return wake.backendHostPort
}

// readWakeHoldResponses passes on the message ID of each login plugin response read from a held
// client, until reading fails or stopped is closed. An error that is not
// errUnexpectedWakeHoldContent occurred between packets, so nothing of the client's was lost.
func readWakeHoldResponses(reader *bufio.Reader, clientAddr net.Addr, responses chan<- int32, stopped <-chan struct{}) error {
	for {
		// Wait for the next packet without consuming any of it. An error here, such as the
		// expired read deadline that ends the hold, leaves the client's stream at a packet
		// boundary, while an error from ReadPacket below may have lost part of a packet.
		if _, err := reader.Peek(1); err != nil {
			return err
		}
		packet, err := mcproto.ReadPacket(reader, clientAddr, mcproto.StateLogin)
		if err != nil {
			return fmt.Errorf("%w: %w", errUnexpectedWakeHoldContent, err)
		}
		if packet.PacketID != mcproto.PacketIdLoginPluginResponse {
			return fmt.Errorf("%w: packet ID %d", errUnexpectedWakeHoldContent, packet.PacketID)
		}
		messageID, _, err := mcproto.DecodeLoginPluginResponse(packet.Data)
		if err != nil {
			return fmt.Errorf("%w: %w", errUnexpectedWakeHoldContent, err)
		}
		select {
		case responses <- messageID:
		case <-stopped:
			return errUnexpectedWakeHoldContent
		}
	}
}
