package server

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

type ScalerFunc func(ctx context.Context) error

var EmptyScalerFunc = func(ctx context.Context) error { return nil }

var tcpShieldPattern = regexp.MustCompile("///.*")

type IRoutes interface {
	Reset()
	RegisterAll(mappings map[string]string)
	// FindBackendForServerAddress returns the host:port for the external server address, if registered.
	// Otherwise, an empty string is returned. Also returns the normalized version of the given serverAddress.
	// The 3rd value returned is an (optional) "waker" function which a caller must invoke to wake up serverAddress.
	// The 4th value returned is an (optional) "sleeper" function which a caller must invoke to shut down serverAddress.
	FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, ScalerFunc, ScalerFunc)
	GetMappings() map[string]string
	DeleteMapping(serverAddress string) bool
	CreateMapping(serverAddress string, backend string, waker ScalerFunc, sleeper ScalerFunc)
	SetDefaultRoute(backend string)
	GetDefaultRoute() string
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
		r.CreateMapping(k, v, EmptyScalerFunc, EmptyScalerFunc)
	}
}

type mapping struct {
	backend string
	waker   ScalerFunc
	sleeper ScalerFunc
}

type routesImpl struct {
	sync.RWMutex
	mappings     map[string]mapping
	defaultRoute string
	simplifySRV  bool
}

func (r *routesImpl) Reset() {
	r.mappings = make(map[string]mapping)
	DownScaler.Reset()
}

func (r *routesImpl) SetDefaultRoute(backend string) {
	r.defaultRoute = backend

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")
}

func (r *routesImpl) GetDefaultRoute() string {
	return r.defaultRoute
}

func (r *routesImpl) SimplifySRV(srvEnabled bool) {
	r.simplifySRV = srvEnabled
}

func (r *routesImpl) FindBackendForServerAddress(_ context.Context, serverAddress string) (string, string, ScalerFunc, ScalerFunc) {
	r.RLock()
	defer r.RUnlock()

	// Trim off Forge null-delimited address parts like \x00FML3\x00
	serverAddress = strings.Split(serverAddress, "\x00")[0]

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
	return r.defaultRoute, serverAddress, nil, nil
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

	DownScaler.Cancel(serverAddress)

	if _, ok := r.mappings[serverAddress]; ok {
		delete(r.mappings, serverAddress)
		return true
	} else {
		return false
	}
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string, waker ScalerFunc, sleeper ScalerFunc) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Created route mapping")
	r.mappings[serverAddress] = mapping{backend: backend, waker: waker, sleeper: sleeper}

	// Trigger auto scale down when mapping is created to ensure servers are shut down if router restarts
	DownScaler.Begin(serverAddress)
}
