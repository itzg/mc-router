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

// CachedStatus holds the cached status response for a backend.
type CachedStatus struct {
	Version     mcproto.StatusVersion
	Description mcproto.StatusText
	Favicon     string
	Players     mcproto.StatusPlayers
	LastUpdated time.Time
}

type StatusCache struct {
	mu    sync.RWMutex
	cache map[string]*CachedStatus // key: serverAddress
	ttl   time.Duration
}

func NewStatusCache(ttl time.Duration) *StatusCache {
	return &StatusCache{
		cache: make(map[string]*CachedStatus),
		ttl:   ttl,
	}
}

func (sc *StatusCache) Get(serverAddress string) (*CachedStatus, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	status, ok := sc.cache[serverAddress]
	if !ok || time.Since(status.LastUpdated) > sc.ttl {
		return nil, false
	}
	return status, true
}

func (sc *StatusCache) Set(serverAddress string, status *CachedStatus) {
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
func fetchBackendStatus(serverAddress string) (*CachedStatus, error) {
	address, port, splitErr := net.SplitHostPort(serverAddress)
	if splitErr != nil {
		logrus.
			WithError(splitErr).
			WithField("serverAddress", serverAddress).
			Error("Failed to split server address")
		return nil, splitErr
	}

	portInt, atoiErr := strconv.Atoi(port)
	if atoiErr != nil {
		logrus.
			WithError(atoiErr).
			WithField("serverAddress", serverAddress).
			Error("Failed to convert port to int")
		return nil, atoiErr
	}

	// Create a new pinger instance with the address and port
	pinger := mcpinger.New(address, uint16(portInt), mcpinger.WithTimeout(5*time.Second))

	info, err := pinger.Ping()
	if err != nil {
		logrus.
			WithError(err).
			WithField("serverAddress", serverAddress).
			Error("Failed to ping backend server")
		return nil, err
	}

	return &CachedStatus{
		Version:     mcproto.StatusVersion{Name: info.Version.Name, Protocol: int(info.Version.Protocol)},
		Description: mcproto.StatusText{Text: info.Description.Text},
		Favicon:     info.Favicon,
		Players:     mcproto.StatusPlayers{Max: int(info.Players.Max), Online: 0, Sample: []mcproto.PlayerEntry{}},
		LastUpdated: time.Now(),
	}, nil
}
