package server

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type IDockerWatcher interface {
	Start(ctx context.Context) error
}

const (
	DockerRouterLabelHost                      = "mc-router.host"
	DockerRouterLabelPort                      = "mc-router.port"
	DockerRouterLabelDefault                   = "mc-router.default"
	DockerRouterLabelNetwork                   = "mc-router.network"
	DockerRouterLabelAutoScaleUp               = "mc-router.auto-scale-up"
	DockerRouterLabelAutoScaleDown             = "mc-router.auto-scale-down"
	DockerRouterLabelAutoScaleAsleepMOTD       = "mc-router.auto-scale-asleep-motd"
	DockerRouterLabelAutoScaleLoadingMOTD      = "mc-router.auto-scale-loading-motd"
	DockerRouterLabelAutoScaleWaitTimeout      = "mc-router.auto-scale-wait-timeout"
	DockerRouterLabelAutoScaleFailedMOTD       = "mc-router.auto-scale-failed-motd"
	DockerRouterLabelAutoScaleRestartDelayMOTD = "mc-router.auto-scale-restart-delay-motd"
)

const containerIdLen = 12

type dockerWatcherConfig struct {
	autoScaleUp   bool
	autoScaleDown bool

	socket     string
	timeout    time.Duration
	apiVersion string
}

func (c *dockerWatcherConfig) apiVersionOpt() client.Opt {
	if c.apiVersion != "" {
		logrus.WithField("apiVersion", c.apiVersion).Debug("Using specific Docker API version")
		return client.WithVersion(c.apiVersion)
	}

	logrus.Debug("Using Docker API version negotiation")
	return client.WithAPIVersionNegotiation()
}

func NewDockerWatcher(socket string, timeout time.Duration, autoScaleUp bool, autoScaleDown bool, dockerApiVersion string, routes IRoutes) IDockerWatcher {
	return &dockerWatcherImpl{
		config: dockerWatcherConfig{
			socket:        socket,
			timeout:       timeout,
			autoScaleUp:   autoScaleUp,
			autoScaleDown: autoScaleDown,
			apiVersion:    dockerApiVersion,
		},
		routes: routes,
	}
}

type dockerWatcherImpl struct {
	sync.RWMutex
	config       dockerWatcherConfig
	client       *client.Client
	containerMap map[string]*routableContainer
	monitorLock  sync.Mutex
	routes       IRoutes
}

func (w *dockerWatcherImpl) makeWakerFunc(rc *routableContainer) WakerFunc {
	if rc == nil || !rc.autoScaleUp {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		containerId := rc.containerId
		if containerId == "" {
			return "", fmt.Errorf("missing container id for wake")
		}
		inspect, err := w.client.ContainerInspect(ctx, containerId)
		if err != nil {
			return "", err
		}
		if inspect.State == nil {
			return "", fmt.Errorf("unable to determine container state")
		}
		// If paused, unpause; if not running, start; otherwise no-op
		if inspect.State.Paused {
			logrus.WithFields(logrus.Fields{"containerId": containerId}).Debug("Unpausing container for wake")
			if err := w.client.ContainerUnpause(ctx, containerId); err != nil {
				return "", err
			}
		} else if !inspect.State.Running {
			logrus.WithFields(logrus.Fields{"containerId": containerId}).Debug("Starting container for wake")
			if err := w.client.ContainerStart(ctx, containerId, container.StartOptions{}); err != nil {
				return "", err
			}
		}

		inspect, err = w.client.ContainerInspect(ctx, containerId)
		if err != nil {
			return "", err
		}
		data, ok := w.parseContainerData(&inspect)
		if !ok {
			return "", fmt.Errorf("failed to parse container data after starting")
		}
		if data.ip == "" {
			return "", fmt.Errorf("container has no accessible IP after starting")
		}
		endpoint := net.JoinHostPort(data.ip, strconv.Itoa(int(data.port)))

		// Route table updates via Docker `start`/`network connect` events.

		// Wait until the container is reachable
		deadline := time.Now().Add(60 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", endpoint, 1*time.Second)
			if err == nil {
				_ = conn.Close()
				break
			}
			if ctx.Err() != nil {
				return endpoint, ctx.Err()
			}
			if time.Now().After(deadline) {
				return endpoint, fmt.Errorf("timeout waiting for container to become reachable at %s", endpoint)
			}
			select {
			case <-ctx.Done():
				return endpoint, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		return endpoint, nil
	}
}

func (w *dockerWatcherImpl) makeSleeperFunc(rc *routableContainer) SleeperFunc {
	if rc == nil || !rc.autoScaleDown {
		return nil
	}
	return func(ctx context.Context) error {
		containerId := rc.containerId
		if containerId == "" {
			return fmt.Errorf("missing container id for sleep")
		}
		inspect, err := w.client.ContainerInspect(ctx, containerId)
		if err != nil {
			return err
		}
		if inspect.State != nil && inspect.State.Running {
			// Graceful stop with 60s timeout
			timeout := 60
			logrus.WithFields(logrus.Fields{"containerId": containerId}).Debug("Stopping container for sleep")
			if err := w.client.ContainerStop(ctx, containerId, container.StopOptions{Timeout: &timeout}); err != nil {
				return err
			}
		}
		// Route table updates via Docker `die`/`stop`/`network disconnect` events.
		return nil
	}
}

// monitorContainers does a full re-list of Docker containers and reconciles
// the route table against it. Used for initial sync at startup and for
// resync after the event stream reconnects (to catch any events missed
// during disconnect).
func (w *dockerWatcherImpl) monitorContainers(ctx context.Context) error {
	w.monitorLock.Lock()
	defer w.monitorLock.Unlock()

	logrus.Trace("Listing Docker containers")
	containers, err := w.listContainers(ctx)
	if err != nil {
		logrus.WithError(err).Error("Docker failed to list containers")
		return err
	}

	byID := map[string][]*routableContainer{}
	for _, rc := range containers {
		byID[rc.containerId] = append(byID[rc.containerId], rc)
	}

	for id, desired := range byID {
		w.applyContainerRoutesLocked(id, desired)
	}

	// Remove entries whose container is no longer present at all
	for name, rc := range w.containerMap {
		if _, present := byID[rc.containerId]; present {
			continue
		}
		delete(w.containerMap, name)
		if name != "" {
			w.routes.RemoveMapping(name)
		} else {
			w.routes.RemoveDefaultRoute()
		}
		logrus.WithField("routableContainer", rc).Debug("DELETE")
	}
	return nil
}

// applyEvent reacts to a single Docker event by reconciling only the routes
// belonging to the affected container — no full re-list.
func (w *dockerWatcherImpl) applyEvent(ctx context.Context, ev events.Message) error {
	containerId := ev.Actor.ID
	if ev.Type == events.NetworkEventType {
		containerId = ev.Actor.Attributes["container"]
	}
	if containerId == "" {
		logrus.WithField("event", ev).Warn("network event missing container attribute, skipping")
		return nil
	}

	var desired []*routableContainer
	if !(ev.Type == events.ContainerEventType && ev.Action == events.ActionDestroy) {
		got, err := w.containersForID(ctx, containerId)
		if err != nil {
			return err
		}
		desired = got
	}

	w.monitorLock.Lock()
	defer w.monitorLock.Unlock()

	// Only trace events that affect a routed container — either one we already
	// track or one becoming routable now. Filters out unrelated daemon noise.
	relevant := len(desired) > 0
	if !relevant {
		for _, rc := range w.containerMap {
			if rc.containerId == containerId {
				relevant = true
				break
			}
		}
	}
	if relevant {
		logrus.WithFields(logrus.Fields{"type": ev.Type, "action": ev.Action, "id": containerId}).Trace("Docker event")
	}

	w.applyContainerRoutesLocked(containerId, desired)
	return nil
}

// containersForID inspects a single container and returns the routableContainers
// it should produce. Returns nil if the container is gone or not routable.
func (w *dockerWatcherImpl) containersForID(ctx context.Context, containerId string) ([]*routableContainer, error) {
	inspect, err := w.client.ContainerInspect(ctx, containerId)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	data, ok := w.parseContainerData(&inspect)
	if !ok {
		return nil, nil
	}
	endpoint := ""
	if !data.notRunning {
		endpoint = dockerBackendEndpoint(data.ip, data.port)
	}
	var result []*routableContainer
	scalingTarget := NewDockerScalingTarget(containerId, data.name)
	for _, host := range data.hosts {
		result = append(result, &routableContainer{
			containerEndpoint:    endpoint,
			externalName:         host,
			containerId:          containerId,
			autoScaleUp:          data.autoScaleUp,
			autoScaleDown:        data.autoScaleDown,
			autoScaleAsleepMOTD:  data.autoScaleAsleepMOTD,
			autoScaleLoadingMOTD: data.autoScaleLoadingMOTD,
			scalingTarget:        scalingTarget,
		})
	}
	if data.def != nil && *data.def {
		result = append(result, &routableContainer{
			containerEndpoint:    endpoint,
			externalName:         "",
			containerId:          containerId,
			autoScaleUp:          data.autoScaleUp,
			autoScaleDown:        data.autoScaleDown,
			autoScaleAsleepMOTD:  data.autoScaleAsleepMOTD,
			autoScaleLoadingMOTD: data.autoScaleLoadingMOTD,
			scalingTarget:        scalingTarget,
		})
	}
	return result, nil
}

// applyContainerRoutesLocked reconciles the routes for a single containerId
// against the desired set. Caller must hold monitorLock.
func (w *dockerWatcherImpl) applyContainerRoutesLocked(containerId string, desired []*routableContainer) {
	desiredByName := map[string]*routableContainer{}
	for _, rc := range desired {
		desiredByName[rc.externalName] = rc
	}

	// Drop entries previously owned by this container that are no longer desired
	for name, rc := range w.containerMap {
		if rc.containerId != containerId {
			continue
		}
		if _, keep := desiredByName[name]; keep {
			continue
		}
		delete(w.containerMap, name)
		if name != "" {
			w.routes.RemoveMapping(name)
		} else {
			w.routes.RemoveDefaultRoute()
		}
		logrus.WithField("routableContainer", rc).Debug("DELETE")
	}

	for _, rs := range desired {
		oldRs, exists := w.containerMap[rs.externalName]
		if !exists {
			w.containerMap[rs.externalName] = rs
			wakerFunc := w.makeWakerFunc(rs)
			sleeperFunc := w.makeSleeperFunc(rs)
			if rs.externalName != "" {
				w.routes.CreateMapping(rs.externalName, rs.containerEndpoint, rs.scalingTarget, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			} else {
				w.routes.SetDefaultRoute(rs.containerEndpoint, rs.scalingTarget, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			}
			logrus.WithField("routableContainer", rs).Debug("ADD")
			continue
		}
		if oldRs.containerEndpoint == rs.containerEndpoint &&
			oldRs.containerId == rs.containerId &&
			oldRs.autoScaleUp == rs.autoScaleUp &&
			oldRs.autoScaleDown == rs.autoScaleDown &&
			oldRs.autoScaleAsleepMOTD == rs.autoScaleAsleepMOTD &&
			oldRs.autoScaleLoadingMOTD == rs.autoScaleLoadingMOTD {
			continue
		}
		w.containerMap[rs.externalName] = rs
		wakerFunc := w.makeWakerFunc(rs)
		sleeperFunc := w.makeSleeperFunc(rs)
		if rs.externalName != "" {
			w.routes.UpdateMapping(rs.externalName, rs.containerEndpoint, rs.scalingTarget, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
		} else {
			w.routes.SetDefaultRoute(rs.containerEndpoint, rs.scalingTarget, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
		}
		logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
	}
}

func (w *dockerWatcherImpl) Start(ctx context.Context) error {
	var err error

	opts := []client.Opt{
		client.FromEnv,
		client.WithTimeout(w.config.timeout),
		client.WithHTTPHeaders(map[string]string{
			"User-Agent": "mc-router ",
		}),
		w.config.apiVersionOpt(),
	}
	if w.config.socket != "" {
		opts = append(opts, client.WithHost(w.config.socket))
	}

	w.client, err = client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}
	w.containerMap = map[string]*routableContainer{}

	logrus.Trace("Performing initial listing of Docker containers")
	if err := w.monitorContainers(ctx); err != nil {
		return err
	}

	// streamEvents will resync on (re)connect and otherwise apply incremental
	// updates from the Docker event stream — no periodic polling.
	go w.streamEvents(ctx)

	logrus.Info("Monitoring Docker for Minecraft containers")
	return nil
}

// streamEvents subscribes to the Docker event stream and triggers reconciliation
// of routes whenever container or network events relevant to routing occur.
// Reconnects with backoff on stream errors (e.g. daemon restart).
func (w *dockerWatcherImpl) streamEvents(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			logrus.Debug("Stopping Docker monitoring")
			return
		}

		eventFilters := filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("type", string(events.NetworkEventType)),
			filters.Arg("event", string(events.ActionStart)),
			filters.Arg("event", string(events.ActionUnPause)),
			filters.Arg("event", string(events.ActionStop)),
			filters.Arg("event", string(events.ActionDie)),
			filters.Arg("event", string(events.ActionPause)),
			filters.Arg("event", string(events.ActionDestroy)),
			filters.Arg("event", string(events.ActionRename)),
			filters.Arg("event", string(events.ActionConnect)),
			filters.Arg("event", string(events.ActionDisconnect)),
		)

		eventCh, errCh := w.client.Events(ctx, events.ListOptions{Filters: eventFilters})

		// Resync after (re)connecting in case we missed events while disconnected
		if err := w.monitorContainers(ctx); err != nil {
			logrus.WithError(err).Error("Docker resync failed")
		} else {
			backoff = time.Second
		}

	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-eventCh:
				if !ok {
					break loop
				}
				if err := w.applyEvent(ctx, ev); err != nil {
					logrus.WithError(err).Error("Docker event handling failed")
				}
			case err, ok := <-errCh:
				if !ok {
					break loop
				}
				if ctx.Err() != nil {
					return
				}
				logrus.WithError(err).Warn("Docker event stream error, reconnecting")
				break loop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (w *dockerWatcherImpl) listContainers(ctx context.Context) ([]*routableContainer, error) {
	containers, err := w.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var result []*routableContainer
	for _, c := range containers {
		containerId := c.ID
		inspect, err := w.client.ContainerInspect(ctx, containerId)
		if err != nil {
			logrus.WithFields(logrus.Fields{"containerId": containerId}).WithError(err).Error("Failed to inspect Docker container")
			continue
		}
		data, ok := w.parseContainerData(&inspect)
		if !ok {
			continue
		}

		endpoint := ""
		if !data.notRunning {
			endpoint = dockerBackendEndpoint(data.ip, data.port)
		}
		logrus.WithField("backendEndpoint", endpoint).
			WithField("containerId", shortContainerId(containerId)).
			Debug("Found routable Docker container")

		scalingTarget := NewDockerScalingTarget(containerId, data.name)
		for _, host := range data.hosts {
			result = append(result, newRoutableContainer(endpoint, host, containerId, data, scalingTarget))
		}
		if data.def != nil && *data.def {
			result = append(result, newRoutableContainer(endpoint, "", containerId, data, scalingTarget))
		}
	}

	return result, nil
}

func newRoutableContainer(endpoint string, host string, containerId string, data parsedDockerContainerData, target *DockerScalingTarget) *routableContainer {
	return &routableContainer{
		containerEndpoint:    endpoint,
		externalName:         host,
		containerId:          containerId,
		autoScaleUp:          data.autoScaleUp,
		autoScaleDown:        data.autoScaleDown,
		autoScaleAsleepMOTD:  data.autoScaleAsleepMOTD,
		autoScaleLoadingMOTD: data.autoScaleLoadingMOTD,
		scalingTarget:        target,
	}
}

type parsedDockerContainerData struct {
	name                 string
	hosts                []string
	port                 uint64
	def                  *bool
	network              *string
	ip                   string
	autoScaleDown        bool
	autoScaleUp          bool
	autoScaleAsleepMOTD  string
	autoScaleLoadingMOTD string
	notRunning           bool
}

func dockerEndpointIP(endpoint *network.EndpointSettings) string {
	if endpoint == nil {
		return ""
	}
	if endpoint.IPAddress != "" {
		return endpoint.IPAddress
	}
	return endpoint.GlobalIPv6Address
}

func dockerBackendEndpoint(ip string, port uint64) string {
	return net.JoinHostPort(ip, strconv.FormatUint(port, 10))
}

func (w *dockerWatcherImpl) parseContainerData(container *container.InspectResponse) (data parsedDockerContainerData, ok bool) {
	data.autoScaleUp = w.config.autoScaleUp
	data.autoScaleDown = w.config.autoScaleDown
	data.name = container.Name
	for key, value := range container.Config.Labels {
		if key == DockerRouterLabelHost {
			if data.hosts != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelHost)
				return
			}
			data.hosts = SplitExternalHosts(value)
		}

		if key == DockerRouterLabelPort {
			if data.port != 0 {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelPort)
				return
			}
			var err error
			data.port, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					WithError(err).
					Warnf("ignoring container with invalid %s label", DockerRouterLabelPort)
				return
			}
		}
		if key == DockerRouterLabelDefault {
			if data.def != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelDefault)
				return
			}
			defaultValue, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					WithError(err).
					Warnf("ignoring container with invalid value for %s label", DockerRouterLabelDefault)
				return
			}
			data.def = &defaultValue
		}
		if key == DockerRouterLabelNetwork {
			if data.network != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelNetwork)
				return
			}
			data.network = new(string)
			*data.network = value
		}
		if key == DockerRouterLabelAutoScaleUp {
			autoScaleUp, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					WithError(err).
					Warnf("ignoring container with invalid value for %s label", DockerRouterLabelAutoScaleUp)
				return
			}
			data.autoScaleUp = autoScaleUp
		}
		if key == DockerRouterLabelAutoScaleDown {
			autoScaleDown, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
					WithError(err).
					Warnf("ignoring container with invalid value for %s label", DockerRouterLabelAutoScaleDown)
				return
			}
			data.autoScaleDown = autoScaleDown
		}
		if key == DockerRouterLabelAutoScaleAsleepMOTD {
			data.autoScaleAsleepMOTD = value
		}
		if key == DockerRouterLabelAutoScaleLoadingMOTD {
			data.autoScaleLoadingMOTD = value
		}
	}

	// probably not minecraft related
	if len(data.hosts) == 0 {
		return
	}

	if len(container.NetworkSettings.Networks) == 0 {
		logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
			Warnf("ignoring container, no networks found")
		return
	}

	if data.port == 0 {
		data.port = 25565
	}

	if data.network != nil {
		// Loop through all the container's networks and attempt to find one whose Network ID, Name, or Aliases match the
		// specified network
		for name, endpoint := range container.NetworkSettings.Networks {
			if name == endpoint.NetworkID {
				data.ip = dockerEndpointIP(endpoint)
			}

			if name == *data.network {
				data.ip = dockerEndpointIP(endpoint)
				break
			}

			if slices.Contains(endpoint.Aliases, name) {
				data.ip = dockerEndpointIP(endpoint)
			}
		}
	} else {
		// If there's no endpoint specified we can just assume the only one is the network we should use. One caveat is
		// if there's more than one network on this container, we should require that the user specifies a network to avoid
		// weird problems.
		if len(container.NetworkSettings.Networks) > 1 {
			logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
				Warnf("ignoring container, multiple networks found and none specified using label %s", DockerRouterLabelNetwork)
			return
		}

		for _, endpoint := range container.NetworkSettings.Networks {
			data.ip = dockerEndpointIP(endpoint)
			break
		}
	}

	if data.ip == "" && container.State != nil && container.State.Running {
		logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
			Warnf("ignoring container, unable to find accessible ip address")
		return
	}

	if container.State != nil && !container.State.Running {
		if !w.config.autoScaleUp {
			logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Name}).
				Debugf("ignoring container, not running and auto scale up is disabled")
			return
		}
		data.notRunning = true
	}

	ok = true

	return
}

type routableContainer struct {
	ScalingIndicator
	externalName         string
	containerEndpoint    string
	containerId          string
	autoScaleUp          bool
	autoScaleDown        bool
	autoScaleAsleepMOTD  string
	autoScaleLoadingMOTD string
	scalingTarget        *DockerScalingTarget
}

func (r *routableContainer) String() string {
	return fmt.Sprintf("routableContainer{externalName=%s, endpoint=%s, id=%s, autoScaleUp=%t, autoScaleDown=%t, autoScaleAsleepMOTD=%s, autoScaleLoadingMOTD=%s, scalingTarget=%v}",
		r.externalName, r.containerEndpoint, shortContainerId(r.containerId), r.autoScaleUp, r.autoScaleDown, r.autoScaleAsleepMOTD, r.autoScaleLoadingMOTD, r.scalingTarget)
}

func shortContainerId(containerId string) string {
	return containerId[0:containerIdLen]
}

type DockerScalingTarget struct {
	ScalingIndicator
	containerId string
	name        string
}

func NewDockerScalingTarget(containerId, name string) *DockerScalingTarget {
	return &DockerScalingTarget{
		ScalingIndicator: ScalingIndicator{scaling: &atomic.Bool{}},
		containerId:      containerId,
		name:             name,
	}
}

func (t *DockerScalingTarget) String() string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("docker{containerId=%s, name=%s}", shortContainerId(t.containerId), t.name)
}

func (t *DockerScalingTarget) ScalingKey() string {
	if t == nil {
		return ""
	}
	return t.containerId
}
