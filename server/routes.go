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

func buildWakerFromSleeper(endpoint string, sleeper SleeperFunc) WakerFunc {
	if sleeper == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		if err := sleeper(ctx); err != nil {
			return "", err
		}
		return endpoint, nil
	}
}

var tcpShieldPattern = regexp.MustCompile("///.*")

// RouteFinder implementations find new routes in the system that can be tracked by a RoutesHandler
type RouteFinder interface {
	Start(ctx context.Context, handler RoutesHandler) error
	String() string
}

type RoutesHandler interface {
	CreateMapping(serverAddress string, backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	SetDefaultRoute(backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	// DeleteMapping requests that the serverAddress be removed from routes.
	// Returns true if the route existed.
	DeleteMapping(serverAddress string) bool
}

type RoutesListener interface {
	// OnRouteAdded is called when a new route is added.
	OnRouteAdded(serverAddress string, backend string)
	// OnDefaultRouteSet is called when a default route is set.
	OnDefaultRouteSet(backend string)
	// OnRouteRemoved is called when a route is removed.
	OnRouteRemoved(serverAddress string)
	// OnDefaultRouteRemoved is called when a default route is removed (or un-set).
	OnDefaultRouteRemoved()
}

type IRoutes interface {
	RoutesHandler

	Reset()
	// FindBackendForServerAddress returns the host:port for the external server address, if registered.
	// Otherwise, an empty string is returned. Also returns the normalized version of the given serverAddress.
	// The 3rd value returned is the scalingTarget which indicates what endpoint to scale (may differ from backend when using proxy).
	// The 4th value returned is an (optional) "waker" function which a caller must invoke to wake up serverAddress.
	// The 5th value returned is an (optional) "sleeper" function which a caller must invoke to shut down serverAddress.
	FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, string, WakerFunc, SleeperFunc)
	HasRoute(serverAddress string) bool
	GetSleepers(scalingTarget string) []SleeperFunc
	GetMappings() map[string]string
	GetDefaultRoute() (string, string, WakerFunc, SleeperFunc)
	GetAsleepMOTD(serverAddress string) string
	GetLoadingMOTD(serverAddress string) string
	SimplifySRV(srvEnabled bool)
	// BulkRegister registers a set of static mappings, attaching the scaler's waker/sleeper pair. nil-safe: a nil scaler registers without autoscaling.
	// Reset must be called separately and previous to this if you want to clear existing mappings.
	BulkRegister(scaler *WebhookScaler, mappings map[string]string)

	WithDownScaler(downScaler IDownScaler) IRoutes
	WithListener(listener RoutesListener) IRoutes
}

func NewRoutes(ctx context.Context) IRoutes {
	r := &routesImpl{
		ctx:      ctx,
		mappings: make(map[string]mapping),
	}
	return r
}

type mapping struct {
	backend       string
	waker         WakerFunc
	sleeper       SleeperFunc
	asleepMOTD    string
	loadingMOTD   string
	scalingTarget string // The endpoint to scale (may differ from backend when using proxy)
}

type routesImpl struct {
	sync.RWMutex
	ctx             context.Context
	mappings        map[string]mapping
	defaultRoute    mapping
	simplifySRV     bool
	downScaler      IDownScaler
	routesListeners []RoutesListener
}

// WithDownScaler sets the optional down scaler for the routes. The down scaler is used to scale down servers when they are no longer needed.
// TODO this is a code smell because it creates a circular dependency between routes and down scaler. The down scaler needs to know about the routes to scale down servers, but the routes also need to know about the down scaler to start scaling down servers when they are no longer needed. This should be refactored in the future.
func (r *routesImpl) WithDownScaler(downScaler IDownScaler) IRoutes {
	r.downScaler = downScaler
	return r
}

// WithListener adds a listener to the routes. The listener will be notified of route changes.
// It will also be notified of existing routes when added. This ensures listeners get a consistent and complete view of routes.
func (r *routesImpl) WithListener(listener RoutesListener) IRoutes {
	r.Lock()
	defer r.Unlock()

	r.routesListeners = append(r.routesListeners, listener)
	for server, backend := range r.mappings {
		listener.OnRouteAdded(server, backend.backend)
	}
	if r.defaultRoute.backend != "" {
		listener.OnDefaultRouteSet(r.defaultRoute.backend)
	}
	return r
}

func (r *routesImpl) Reset() {
	r.Lock()
	defer r.Unlock()

	for serverAddress := range r.mappings {
		for _, listener := range r.routesListeners {
			listener.OnRouteRemoved(serverAddress)
		}
	}

	r.mappings = make(map[string]mapping)

	for _, listener := range r.routesListeners {
		listener.OnDefaultRouteRemoved()
	}

	if r.downScaler != nil {
		r.downScaler.Reset()
	}
}

func (r *routesImpl) SetDefaultRoute(backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	if scalingTarget == "" {
		scalingTarget = backend
	}
	r.defaultRoute = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")

	for _, listener := range r.routesListeners {
		listener.OnDefaultRouteSet(backend)
	}
}

func (r *routesImpl) GetDefaultRoute() (string, string, WakerFunc, SleeperFunc) {
	return r.defaultRoute.backend, r.defaultRoute.scalingTarget, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) GetAsleepMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		return r.defaultRoute.asleepMOTD
	}

	if m, ok := r.mappings[serverAddress]; ok {
		return m.asleepMOTD
	}
	return ""
}

func (r *routesImpl) GetLoadingMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		return r.defaultRoute.loadingMOTD
	}

	if m, ok := r.mappings[serverAddress]; ok {
		return m.loadingMOTD
	}
	return ""
}

func (r *routesImpl) SimplifySRV(srvEnabled bool) {
	r.simplifySRV = srvEnabled
}

func (r *routesImpl) HasRoute(serverAddress string) bool {
	r.RLock()
	defer r.RUnlock()

	_, exists := r.mappings[serverAddress]
	return exists
}

func (r *routesImpl) FindBackendForServerAddress(_ context.Context, serverAddress string) (string, string, string, WakerFunc, SleeperFunc) {
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
			return mapping.backend, serverAddress, mapping.scalingTarget, mapping.waker, mapping.sleeper
		}
	}
	return r.defaultRoute.backend, serverAddress, r.defaultRoute.scalingTarget, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) GetSleepers(scalingTarget string) []SleeperFunc {
	r.RLock()
	defer r.RUnlock()

	var sleepers []SleeperFunc
	for _, m := range r.mappings {
		if m.scalingTarget == scalingTarget && m.sleeper != nil {
			sleepers = append(sleepers, m.sleeper)
		}
	}
	if r.defaultRoute.scalingTarget == scalingTarget && r.defaultRoute.sleeper != nil {
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
		r.downScaler.Cancel(m.scalingTarget)
		delete(r.mappings, serverAddress)

		for _, listener := range r.routesListeners {
			listener.OnRouteRemoved(serverAddress)
		}

		return true
	} else {
		return false
	}
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	if scalingTarget == "" {
		scalingTarget = backend
	}

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Created route mapping")
	r.mappings[serverAddress] = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	for _, listener := range r.routesListeners {
		listener.OnRouteAdded(serverAddress, backend)
	}

	// Trigger auto scale down when mapping is created to ensure servers are shut down if router restarts
	if r.downScaler != nil && scalingTarget != "" {
		r.downScaler.Start(r.ctx, scalingTarget, r)
	}
}

func (r *routesImpl) BulkRegister(scaler *WebhookScaler, mappings map[string]string) {
	for k, v := range mappings {
		waker, sleeper := scaler.routeFuncs(k, v)
		r.CreateMapping(k, v, "", waker, sleeper, "", "")
	}
}
