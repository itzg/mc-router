package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type IDockerWatcher interface {
	Start(ctx context.Context) error
}

const (
	DockerRouterLabelHost                = "mc-router.host"
	DockerRouterLabelPort                = "mc-router.port"
	DockerRouterLabelDefault             = "mc-router.default"
	DockerRouterLabelNetwork             = "mc-router.network"
	DockerRouterLabelAutoScaleUp         = "mc-router.auto-scale-up"
	DockerRouterLabelAutoScaleDown       = "mc-router.auto-scale-down"
	DockerRouterLabelAutoScaleAsleepMOTD = "mc-router.auto-scale-asleep-motd"
)

type dockerWatcherConfig struct {
	autoScaleUp            bool
	autoScaleDown          bool
	socket                 string
	timeoutSeconds         int
	refreshIntervalSeconds int
	apiVersion             string
}

func (c *dockerWatcherConfig) apiVersionOpt() client.Opt {
	if c.apiVersion != "" {
		logrus.WithField("apiVersion", c.apiVersion).Debug("Using specific Docker API version")
		return client.WithVersion(c.apiVersion)
	} else {
		logrus.Debug("Using Docker API version negotiation")
		return client.WithAPIVersionNegotiation()
	}
}

func NewDockerWatcher(socket string, timeoutSeconds int, refreshIntervalSeconds int, autoScaleUp bool, autoScaleDown bool, dockerApiVersion string) IDockerWatcher {
	return &dockerWatcherImpl{
		config: dockerWatcherConfig{
			socket:                 socket,
			timeoutSeconds:         timeoutSeconds,
			refreshIntervalSeconds: refreshIntervalSeconds,
			autoScaleUp:            autoScaleUp,
			autoScaleDown:          autoScaleDown,
			apiVersion:             dockerApiVersion,
		},
	}
}

type dockerWatcherImpl struct {
	sync.RWMutex
	config           dockerWatcherConfig
	client           *client.Client
	dockerRoutesLock sync.RWMutex
}

func (w *dockerWatcherImpl) makeWakerFunc(rc *routableContainer) WakerFunc {
	if rc == nil || !rc.autoScaleUp {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		containerID := rc.containerID
		if containerID == "" {
			return "", fmt.Errorf("missing container id for wake")
		}
		inspect, err := w.client.ContainerInspect(ctx, containerID)
		if err != nil {
			return "", err
		}
		if inspect.State == nil {
			return "", fmt.Errorf("unable to determine container state")
		}
		// If paused, unpause; if not running, start; otherwise no-op
		if inspect.State.Paused {
			logrus.WithFields(logrus.Fields{"containerID": containerID}).Debug("Unpausing container for wake")
			if err := w.client.ContainerUnpause(ctx, containerID); err != nil {
				return "", err
			}
		} else if !inspect.State.Running {
			logrus.WithFields(logrus.Fields{"containerID": containerID}).Debug("Starting container for wake")
			if err := w.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
				return "", err
			}
		}

		inspect, err = w.client.ContainerInspect(ctx, containerID)
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
		containerID := rc.containerID
		if containerID == "" {
			return fmt.Errorf("missing container id for sleep")
		}
		inspect, err := w.client.ContainerInspect(ctx, containerID)
		if err != nil {
			return err
		}
		if inspect.State != nil && inspect.State.Running {
			// Graceful stop with 60s timeout
			timeout := 60
			logrus.WithFields(logrus.Fields{"containerID": containerID}).Debug("Stopping container for sleep")
			if err := w.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
				return err
			}
		}
		return nil
	}
}

func (w *dockerWatcherImpl) Start(ctx context.Context) error {
	var err error

	timeout := time.Duration(w.config.timeoutSeconds) * time.Second
	refreshInterval := time.Duration(w.config.refreshIntervalSeconds) * time.Second

	opts := []client.Opt{
		client.WithHost(w.config.socket),
		client.WithTimeout(timeout),
		client.WithHTTPHeaders(map[string]string{
			"User-Agent": "mc-router ",
		}),
		w.config.apiVersionOpt(),
	}

	w.client, err = client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(refreshInterval)

	logrus.Trace("Performing initial listing of Docker containers")
	initialContainers, err := w.listContainers(ctx)
	if err != nil {
		return err
	}

	w.dockerRoutesLock.Lock()
	containerMap := map[string]*routableContainer{}
	for _, c := range initialContainers {
		containerMap[c.externalContainerName] = c
		wakerFunc := w.makeWakerFunc(c)
		sleeperFunc := w.makeSleeperFunc(c)
		if c.externalContainerName != "" {
			Routes.CreateMapping(c.externalContainerName, c.containerEndpoint, wakerFunc, sleeperFunc, c.autoScaleAsleepMOTD)
		} else {
			Routes.SetDefaultRoute(c.containerEndpoint, wakerFunc, sleeperFunc, c.autoScaleAsleepMOTD)
		}
	}
	w.dockerRoutesLock.Unlock()

	go func() {
		for {
			select {
			case <-ticker.C:
				logrus.Trace("Listing Docker containers")
				containers, err := w.listContainers(ctx)
				if err != nil {
					logrus.WithError(err).Error("Docker failed to list containers")
					return
				}

				visited := map[string]struct{}{}
				w.dockerRoutesLock.Lock()
				for _, rs := range containers {
					if oldRs, ok := containerMap[rs.externalContainerName]; !ok {
						containerMap[rs.externalContainerName] = rs
						logrus.WithField("routableContainer", rs).Debug("ADD")
						wakerFunc := w.makeWakerFunc(rs)
						sleeperFunc := w.makeSleeperFunc(rs)
						if rs.externalContainerName != "" {
							Routes.CreateMapping(rs.externalContainerName, rs.containerEndpoint, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD)
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD)
						}
					} else if oldRs.containerEndpoint != rs.containerEndpoint ||
						oldRs.containerID != rs.containerID ||
						oldRs.autoScaleUp != rs.autoScaleUp ||
						oldRs.autoScaleDown != rs.autoScaleDown ||
						oldRs.autoScaleAsleepMOTD != rs.autoScaleAsleepMOTD {
						containerMap[rs.externalContainerName] = rs
						wakerFunc := w.makeWakerFunc(rs)
						sleeperFunc := w.makeSleeperFunc(rs)
						if rs.externalContainerName != "" {
							Routes.DeleteMapping(rs.externalContainerName)
							Routes.CreateMapping(rs.externalContainerName, rs.containerEndpoint, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD)
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD)
						}
						logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
					}
					visited[rs.externalContainerName] = struct{}{}
				}
				for _, rs := range containerMap {
					if _, ok := visited[rs.externalContainerName]; !ok {
						delete(containerMap, rs.externalContainerName)
						if rs.externalContainerName != "" {
							Routes.DeleteMapping(rs.externalContainerName)
						} else {
							Routes.SetDefaultRoute("", nil, nil, "")
						}
						logrus.WithField("routableContainer", rs).Debug("DELETE")
					}
				}
				w.dockerRoutesLock.Unlock()

			case <-ctx.Done():
				logrus.Debug("Stopping Docker monitoring")
				ticker.Stop()
				return
			}
		}
	}()

	logrus.Info("Monitoring Docker for Minecraft containers")
	return nil
}

func (w *dockerWatcherImpl) listContainers(ctx context.Context) ([]*routableContainer, error) {
	containers, err := w.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var result []*routableContainer
	for _, container := range containers {
		inspect, err := w.client.ContainerInspect(ctx, container.ID)
		if err != nil {
			logrus.WithFields(logrus.Fields{"containerID": container.ID}).WithError(err).Error("Failed to inspect Docker container")
			continue
		}
		data, ok := w.parseContainerData(&inspect)
		if !ok {
			continue
		}

		endpoint := ""
		if !data.notRunning {
			endpoint = fmt.Sprintf("%s:%d", data.ip, data.port)
		}

		for _, host := range data.hosts {
			result = append(result, &routableContainer{
				containerEndpoint:     endpoint,
				externalContainerName: host,
				containerID:           container.ID,
				autoScaleUp:           data.autoScaleUp,
				autoScaleDown:         data.autoScaleDown,
				autoScaleAsleepMOTD:   data.autoScaleAsleepMOTD,
			})
		}
		if data.def != nil && *data.def {
			result = append(result, &routableContainer{
				containerEndpoint:     endpoint,
				externalContainerName: "",
				containerID:           container.ID,
				autoScaleUp:           data.autoScaleUp,
				autoScaleDown:         data.autoScaleDown,
				autoScaleAsleepMOTD:   data.autoScaleAsleepMOTD,
			})
		}
	}

	return result, nil
}

type parsedDockerContainerData struct {
	hosts               []string
	port                uint64
	def                 *bool
	network             *string
	ip                  string
	autoScaleDown       bool
	autoScaleUp         bool
	autoScaleAsleepMOTD string
	notRunning          bool
}

func (w *dockerWatcherImpl) parseContainerData(container *container.InspectResponse) (data parsedDockerContainerData, ok bool) {
	data.autoScaleUp = w.config.autoScaleUp
	data.autoScaleDown = w.config.autoScaleDown
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
			data.def = new(bool)

			lowerValue := strings.TrimSpace(strings.ToLower(value))
			*data.def = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
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
			lowerValue := strings.TrimSpace(strings.ToLower(value))
			data.autoScaleUp = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
		}
		if key == DockerRouterLabelAutoScaleDown {
			lowerValue := strings.TrimSpace(strings.ToLower(value))
			data.autoScaleDown = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
		}
		if key == DockerRouterLabelAutoScaleAsleepMOTD {
			data.autoScaleAsleepMOTD = value
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
				data.ip = endpoint.IPAddress
			}

			if name == *data.network {
				data.ip = endpoint.IPAddress
				break
			}

			for _, alias := range endpoint.Aliases {
				if alias == name {
					data.ip = endpoint.IPAddress
					break
				}
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
			data.ip = endpoint.IPAddress
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
				Warnf("ignoring container, not running and auto scale up is disabled")
			return
		}
		data.notRunning = true
	}

	ok = true

	return
}

type routableContainer struct {
	externalContainerName string
	containerEndpoint     string
	containerID           string
	autoScaleUp           bool
	autoScaleDown         bool
	autoScaleAsleepMOTD   string
}
