package server

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// WakerFunc is a function that wakes up a server and returns its address.
type WakerFunc func(ctx context.Context) (string, error)

// SleeperFunc is a function that puts a server to sleep.
type SleeperFunc func(ctx context.Context) error

var tcpShieldPattern = regexp.MustCompile("///.*")

// RouteFinder implementations find new routes in the system that can be tracked by a RoutesHandler
type RouteFinder interface {
	Start(ctx context.Context, handler RoutesHandler) error
	String() string
}

type RoutesHandler interface {
	CreateMapping(serverAddress string, backend string, waker WakerFunc, sleeper SleeperFunc)
	SetDefaultRoute(backend string, waker WakerFunc, sleeper SleeperFunc)
	// DeleteMapping requests that the serverAddress be removed from routes.
	// Returns true if the route existed.
	DeleteMapping(serverAddress string) bool
}

type IRoutes interface {
	RoutesHandler

	Reset()
	RegisterAll(mappings map[string]string)
	// FindBackendForServerAddress returns the host:port for the external server address, if registered.
	// Otherwise, an empty string is returned. Also returns the normalized version of the given serverAddress.
	// The 3rd value returned is an (optional) "waker" function which a caller must invoke to wake up serverAddress.
	// The 4th value returned is an (optional) "sleeper" function which a caller must invoke to shut down serverAddress.
	FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, WakerFunc, SleeperFunc)
	GetSleepers(backend string) []SleeperFunc
	GetMappings() map[string]string
	GetDefaultRoute() (string, WakerFunc, SleeperFunc)
	SimplifySRV(srvEnabled bool)
}

var Routes = NewRoutes()

func NewRoutes() IRoutes {
	r := &routesImpl{
		mappings: make(map[string]mapping),
	}

	return r
}

func (r *routesImpl) RegisterAll(mappings map[string]string) {
	for k, v := range mappings {
		r.CreateMapping(k, v, nil, nil)
	}
}

type mapping struct {
	backend string
	waker   WakerFunc
	sleeper SleeperFunc
}

type routesImpl struct {
	sync.RWMutex
	mappings     map[string]mapping
	defaultRoute mapping
	simplifySRV  bool
}

func (r *routesImpl) Reset() {
	r.mappings = make(map[string]mapping)
	DownScaler.Reset()
}

func (r *routesImpl) SetDefaultRoute(backend string, waker WakerFunc, sleeper SleeperFunc) {
	r.defaultRoute = mapping{backend: backend, waker: waker, sleeper: sleeper}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")
}

func (r *routesImpl) GetDefaultRoute() (string, WakerFunc, SleeperFunc) {
	return r.defaultRoute.backend, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) SimplifySRV(srvEnabled bool) {
	r.simplifySRV = srvEnabled
}

func (r *routesImpl) FindBackendForServerAddress(_ context.Context, serverAddress string) (string, string, WakerFunc, SleeperFunc) {
	r.RLock()
	defer r.RUnlock()

	// Trim off Forge null-delimited address parts like \x00FML3\x00
	serverAddress = strings.Split(serverAddress, "\x00")[0]

	// Trim off infinity-filter backslash address parts like \\GUID\\CLIENT_IP...
	serverAddress = strings.Split(serverAddress, "\\")[0]

	serverAddress = strings.ToLower(
		// trim the root zone indicator, see https://en.wikipedia.org/wiki/Fully_qualified_domain_name
		strings.TrimSuffix(serverAddress, "."))

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
	}).Debug("Finding backend for server address")

	if r.simplifySRV {
		parts := strings.Split(serverAddress, ".")
		tcpIndex := -1
		for i, part := range parts {
			if part == "_tcp" {
				tcpIndex = i
				break
			}
		}
		if tcpIndex != -1 {
			parts = parts[tcpIndex+1:]
		}

		serverAddress = strings.Join(parts, ".")
	}

	// Strip suffix of TCP Shield
	serverAddress = tcpShieldPattern.ReplaceAllString(serverAddress, "")

	if r.mappings != nil {
		if mapping, exists := r.mappings[serverAddress]; exists {
			return mapping.backend, serverAddress, mapping.waker, mapping.sleeper
		}
	}
	return r.defaultRoute.backend, serverAddress, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) GetSleepers(backend string) []SleeperFunc {
	r.RLock()
	defer r.RUnlock()

	var sleepers []SleeperFunc
	for _, m := range r.mappings {
		if m.backend == backend && m.sleeper != nil {
			sleepers = append(sleepers, m.sleeper)
		}
	}
	if r.defaultRoute.backend == backend && r.defaultRoute.sleeper != nil {
		sleepers = append(sleepers, r.defaultRoute.sleeper)
	}
	return sleepers
}

func (r *routesImpl) GetMappings() map[string]string {
	r.RLock()
	defer r.RUnlock()

	result := make(map[string]string, len(r.mappings))
	for k, v := range r.mappings {
		result[k] = v.backend
	}
	return result
}

func (r *routesImpl) DeleteMapping(serverAddress string) bool {
	r.Lock()
	defer r.Unlock()
	logrus.WithField("serverAddress", serverAddress).Info("Deleting route")

	if m, ok := r.mappings[serverAddress]; ok {
		DownScaler.Cancel(m.backend)
		delete(r.mappings, serverAddress)
		return true
	} else {
		return false
	}
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string, waker WakerFunc, sleeper SleeperFunc) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Created route mapping")
	r.mappings[serverAddress] = mapping{backend: backend, waker: waker, sleeper: sleeper}

	// Trigger auto scale down when mapping is created to ensure servers are shut down if router restarts
	if backend != "" {
		DownScaler.Begin(backend)
	}
}
