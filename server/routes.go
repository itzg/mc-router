package server

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// TODO make sure WakerFunc and SleeperFunc are guarded by ScalingTarget.StartScaling and EndScaling-
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
	CreateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	// UpdateMapping atomically replaces the backend for an existing route without touching the
	// scale-down timer. Use this instead of RemoveMapping+CreateMapping when the server address
	// itself has not changed, so the timer bounce (cancel-on-remove, start-on-create) is avoided.
	// If backend is empty the timer is cancelled (container stopped externally).
	UpdateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	SetDefaultRoute(backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	RemoveDefaultRoute()
	// RemoveMapping requests that the serverAddress be removed from routes.
	// Returns true if the route existed.
	RemoveMapping(serverAddress string) bool
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
	FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, ScalingTarget, WakerFunc, SleeperFunc)
	HasRoute(serverAddress string) bool
	GetSleeper(scalingTarget ScalingTarget) SleeperFunc
	GetMappings() map[string]string
	GetDefaultRoute() (string, ScalingTarget, WakerFunc, SleeperFunc)
	GetAsleepMOTD(serverAddress string) string
	GetLoadingMOTD(serverAddress string) string
	SetCountdownDeadline(serverAddress string, deadline time.Time)
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
	backend           string
	waker             WakerFunc
	sleeper           SleeperFunc
	asleepMOTD        string
	loadingMOTD       string
	scalingTarget     ScalingTarget // The endpoint to scale (may differ from backend when using proxy)
	countdownDeadline time.Time
}

type routesImpl struct {
	sync.RWMutex
	ctx             context.Context
	mappings        map[string]mapping
	defaultRoute    *mapping
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
	if r.defaultRoute != nil && r.defaultRoute.backend != "" {
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

func (r *routesImpl) SetDefaultRoute(backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	r.defaultRoute = &mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")

	for _, listener := range r.routesListeners {
		listener.OnDefaultRouteSet(backend)
	}

	// Trigger auto-scale down for default route on creation, same as CreateMapping.
	if r.downScaler != nil && scalingTarget != nil && backend != "" {
		r.downScaler.Start(r.ctx, scalingTarget, r)
	}
}

func (r *routesImpl) GetDefaultRoute() (string, ScalingTarget, WakerFunc, SleeperFunc) {
	return r.defaultRoute.backend, r.defaultRoute.scalingTarget, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) RemoveDefaultRoute() {
	r.Lock()
	defer r.Unlock()

	if r.defaultRoute == nil {
		return
	}

	for _, listener := range r.routesListeners {
		listener.OnDefaultRouteRemoved()
	}
}

func formatMOTD(motd string, deadline time.Time) string {
	if !strings.Contains(motd, "{duration}") {
		return motd
	}
	if deadline.IsZero() {
		return strings.ReplaceAll(motd, "{duration}", "now")
	}
	now := time.Now()
	if now.Before(deadline) {
		remaining := deadline.Sub(now)
		durationStr := remaining.Round(time.Second).String()
		return strings.ReplaceAll(motd, "{duration}", durationStr)
	}
	return strings.ReplaceAll(motd, "{duration}", "now")
}

func (r *routesImpl) GetAsleepMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		if r.defaultRoute == nil {
			return ""
		}
		return formatMOTD(r.defaultRoute.asleepMOTD, r.defaultRoute.countdownDeadline)
	}

	serverAddress = strings.ToLower(serverAddress)
	if m, ok := r.mappings[serverAddress]; ok {
		return formatMOTD(m.asleepMOTD, m.countdownDeadline)
	}
	return ""
}

func (r *routesImpl) GetLoadingMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		if r.defaultRoute == nil {
			return ""
		}
		return formatMOTD(r.defaultRoute.loadingMOTD, r.defaultRoute.countdownDeadline)
	}

	serverAddress = strings.ToLower(serverAddress)
	if m, ok := r.mappings[serverAddress]; ok {
		return formatMOTD(m.loadingMOTD, m.countdownDeadline)
	}
	return ""
}

func (r *routesImpl) SetCountdownDeadline(serverAddress string, deadline time.Time) {
	r.Lock()
	defer r.Unlock()

	if serverAddress == "" {
		r.defaultRoute.countdownDeadline = deadline
		return
	}

	serverAddress = strings.ToLower(serverAddress)
	if m, ok := r.mappings[serverAddress]; ok {
		m.countdownDeadline = deadline
		r.mappings[serverAddress] = m
	}
}

func (r *routesImpl) SimplifySRV(srvEnabled bool) {
	r.simplifySRV = srvEnabled
}

func (r *routesImpl) HasRoute(serverAddress string) bool {
	r.RLock()
	defer r.RUnlock()

	serverAddress = strings.ToLower(serverAddress)
	_, exists := r.mappings[serverAddress]
	return exists
}

func (r *routesImpl) FindBackendForServerAddress(_ context.Context, serverAddress string) (string, string, ScalingTarget, WakerFunc, SleeperFunc) {
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
	if r.defaultRoute != nil {
		return r.defaultRoute.backend, serverAddress, r.defaultRoute.scalingTarget, r.defaultRoute.waker, r.defaultRoute.sleeper
	}
	return "", serverAddress, nil, nil, nil
}

func (r *routesImpl) GetSleeper(scalingTarget ScalingTarget) SleeperFunc {
	if scalingTarget == nil {
		return nil
	}

	r.RLock()
	defer r.RUnlock()

	for _, m := range r.mappings {
		if m.scalingTarget != nil && m.scalingTarget.ScalingKey() == scalingTarget.ScalingKey() && m.sleeper != nil {
			return m.sleeper
		}
	}
	if r.defaultRoute != nil && r.defaultRoute.scalingTarget != nil && r.defaultRoute.scalingTarget.ScalingKey() == scalingTarget.ScalingKey() && r.defaultRoute.sleeper != nil {
		return r.defaultRoute.sleeper
	}
	return nil
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

func (r *routesImpl) RemoveMapping(serverAddress string) bool {
	r.Lock()
	defer r.Unlock()
	logrus.WithField("serverAddress", serverAddress).Info("Deleting route")

	serverAddress = strings.ToLower(serverAddress)
	if m, ok := r.mappings[serverAddress]; ok {
		if r.downScaler != nil {
			r.downScaler.Cancel(m.scalingTarget)
		}
		delete(r.mappings, serverAddress)

		for _, listener := range r.routesListeners {
			listener.OnRouteRemoved(serverAddress)
		}

		return true
	}

	return false
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Created route mapping")
	r.mappings[serverAddress] = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	for _, listener := range r.routesListeners {
		listener.OnRouteAdded(serverAddress, backend)
	}

	// Trigger auto-scale down when mapping is created to ensure servers are shut down if router restarts.
	// Only start the timer when backend is non-empty — an empty backend means the server is already
	// asleep/stopped (e.g. a Docker stop event updated the route), so there is nothing to scale down.
	if r.downScaler != nil && scalingTarget != nil && backend != "" {
		r.downScaler.Start(r.ctx, scalingTarget, r)
	}
}

// UpdateMapping atomically replaces the backend for an existing route.
// It will also cancel the down scaler if backend is now "down" and start the down scaler timer
// if the backend is now routable but previous mapping entry wasn't, much like CreateMapping
func (r *routesImpl) UpdateMapping(serverAddress string, backend string, scalingTarget ScalingTarget, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Updated route mapping")
	prev, hasPrevious := r.mappings[serverAddress]
	r.mappings[serverAddress] = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	for _, listener := range r.routesListeners {
		listener.OnRouteRemoved(serverAddress)
		listener.OnRouteAdded(serverAddress, backend)
	}

	if r.downScaler != nil && scalingTarget != nil {
		// Cancel the timer when the backend disappears (container stopped externally).
		if backend == "" {
			r.downScaler.Cancel(scalingTarget)
			// start timer on a backend transition from down/waking to ready
		} else if hasPrevious && prev.backend == "" && !scalingTarget.IsScaling() {
			r.downScaler.Start(r.ctx, scalingTarget, r)
		}
	}
}

func (r *routesImpl) BulkRegister(scaler *WebhookScaler, mappings map[string]string) {
	for k, v := range mappings {
		waker, sleeper, scalingTarget := scaler.routeFuncs(k, v)
		r.CreateMapping(k, v, scalingTarget, waker, sleeper, "", "")
	}
}
