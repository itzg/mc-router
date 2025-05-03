package server

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/itzg/mc-router/mcproto"
	"github.com/sirupsen/logrus"

	mcpinger "github.com/Raqbit/mc-pinger"
)

type StatusCache struct {
	mu    sync.RWMutex
	cache map[string]*mcproto.StatusResponse // key: serverAddress
	ttl   time.Duration
}

func NewStatusCache(ttl time.Duration) *StatusCache {
	return &StatusCache{
		cache: make(map[string]*mcproto.StatusResponse),
		ttl:   ttl,
	}
}

func (sc *StatusCache) Get(serverAddress string) (*mcproto.StatusResponse, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	status, ok := sc.cache[serverAddress]
	if !ok {
		return nil, false
	}
	return status, true
}

func (sc *StatusCache) Set(serverAddress string, status *mcproto.StatusResponse) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.cache[serverAddress] = status
}

func (sc *StatusCache) Delete(serverAddress string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.cache, serverAddress)
}

func (sc *StatusCache) updateAll(getBackends func() map[string]string) {
	for serverAddress, backendAddress := range getBackends() {
		logrus.
			WithField("serverAddress", serverAddress).
			WithField("backendAddress", backendAddress).
			Debug("Updating status cache")

		status, err := fetchBackendStatus(backendAddress)
		if err == nil {
			sc.Set(serverAddress, status)
		}
	}
}

func (sc *StatusCache) StartUpdater(connector *Connector, interval time.Duration, getBackends func() map[string]string) {
	// Update the status cache immediately
	sc.updateAll(getBackends)

	// Start a goroutine to periodically update the status cache
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			<-ticker.C
			sc.updateAll(getBackends)
		}
	}()
}

// fetchBackendStatus connects to the backend and retrieves its status.
func fetchBackendStatus(backendHost string) (*mcproto.StatusResponse, error) {
	address, port, splitErr := net.SplitHostPort(backendHost)
	if splitErr != nil {
		logrus.
			WithError(splitErr).
			WithField("backend", backendHost).
			Error("Failed to split server address")
		return nil, splitErr
	}

	portInt, atoiErr := strconv.Atoi(port)
	if atoiErr != nil {
		logrus.
			WithError(atoiErr).
			WithField("serverAddress", backendHost).
			Error("Failed to convert port to int")
		return nil, atoiErr
	}

	// Create a new pinger instance with the address and port
	pinger := mcpinger.New(address, uint16(portInt), mcpinger.WithTimeout(5*time.Second))

	info, err := pinger.Ping()
	if err != nil {
		logrus.
			WithError(err).
			WithField("backend", backendHost).
			Error("Failed to ping backend server")
		return nil, err
	}

	return &mcproto.StatusResponse{
		Version:     mcproto.StatusVersion{Name: info.Version.Name, Protocol: int(info.Version.Protocol)},
		Description: mcproto.StatusText{Text: info.Description.Text},
		Favicon:     info.Favicon,
		Players:     mcproto.StatusPlayers{Max: int(info.Players.Max), Online: 0, Sample: []mcproto.PlayerEntry{}},
	}, nil
}
