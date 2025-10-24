package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type IDockerWatcher interface {
	Start(ctx context.Context, socket string, timeoutSeconds int, refreshIntervalSeconds int, autoScaleUp bool, autoScaleDown bool) error
}

const (
	DockerAPIVersion         = "1.24"
	DockerRouterLabelHost    = "mc-router.host"
	DockerRouterLabelPort    = "mc-router.port"
	DockerRouterLabelDefault = "mc-router.default"
	DockerRouterLabelNetwork = "mc-router.network"
)

var DockerWatcher IDockerWatcher = &dockerWatcherImpl{}

type dockerWatcherImpl struct {
	sync.RWMutex
	autoScaleUp   bool
	autoScaleDown bool
	client        *client.Client
}

func (w *dockerWatcherImpl) makeWakerFunc(_ *routableContainer) ScalerFunc {
	if !w.autoScaleUp {
		return nil
	}
	return func(ctx context.Context) error {
		logrus.Fatal("Auto scale up is not yet supported for docker")
		return nil
	}
}

func (w *dockerWatcherImpl) makeSleeperFunc(_ *routableContainer) ScalerFunc {
	if !w.autoScaleDown {
		return nil
	}
	return func(ctx context.Context) error {
		logrus.Fatal("Auto scale down is not yet supported for docker")
		return nil
	}
}

func (w *dockerWatcherImpl) Start(ctx context.Context, socket string, timeoutSeconds int, refreshIntervalSeconds int, autoScaleUp bool, autoScaleDown bool) error {
	var err error

	w.autoScaleUp = autoScaleUp
	w.autoScaleDown = autoScaleDown

	timeout := time.Duration(timeoutSeconds) * time.Second
	refreshInterval := time.Duration(refreshIntervalSeconds) * time.Second

	opts := []client.Opt{
		client.WithHost(socket),
		client.WithTimeout(timeout),
		client.WithHTTPHeaders(map[string]string{
			"User-Agent": "mc-router ",
		}),
		client.WithVersion(DockerAPIVersion),
	}

	w.client, err = client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(refreshInterval)
	containerMap := map[string]*routableContainer{}

	logrus.Trace("Performing initial listing of Docker containers")
	initialContainers, err := w.listContainers(ctx)
	if err != nil {
		return err
	}

	for _, c := range initialContainers {
		containerMap[c.externalContainerName] = c
		if c.externalContainerName != "" {
			Routes.CreateMapping(c.externalContainerName, c.containerEndpoint, w.makeWakerFunc(c), w.makeSleeperFunc(c))
		} else {
			Routes.SetDefaultRoute(c.containerEndpoint)
		}
	}

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
				for _, rs := range containers {
					if oldRs, ok := containerMap[rs.externalContainerName]; !ok {
						containerMap[rs.externalContainerName] = rs
						logrus.WithField("routableContainer", rs).Debug("ADD")
						if rs.externalContainerName != "" {
							Routes.CreateMapping(rs.externalContainerName, rs.containerEndpoint, w.makeWakerFunc(rs), w.makeSleeperFunc(rs))
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint)
						}
					} else if oldRs.containerEndpoint != rs.containerEndpoint {
						containerMap[rs.externalContainerName] = rs
						if rs.externalContainerName != "" {
							Routes.DeleteMapping(rs.externalContainerName)
							Routes.CreateMapping(rs.externalContainerName, rs.containerEndpoint, w.makeWakerFunc(rs), w.makeSleeperFunc(rs))
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint)
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
							Routes.SetDefaultRoute("")
						}
						logrus.WithField("routableContainer", rs).Debug("DELETE")
					}
				}

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
	containers, err := w.client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}

	var result []*routableContainer
	for _, container := range containers {
		data, ok := w.parseContainerData(&container)
		if !ok {
			continue
		}

		for _, host := range data.hosts {
			result = append(result, &routableContainer{
				containerEndpoint:     fmt.Sprintf("%s:%d", data.ip, data.port),
				externalContainerName: host,
			})
		}
		if data.def != nil && *data.def {
			result = append(result, &routableContainer{
				containerEndpoint:     fmt.Sprintf("%s:%d", data.ip, data.port),
				externalContainerName: "",
			})
		}
	}

	return result, nil
}

type parsedDockerContainerData struct {
	hosts   []string
	port    uint64
	def     *bool
	network *string
	ip      string
}

func (w *dockerWatcherImpl) parseContainerData(container *dockertypes.Container) (data parsedDockerContainerData, ok bool) {
	for key, value := range container.Labels {
		if key == DockerRouterLabelHost {
			if data.hosts != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelHost)
				return
			}
			data.hosts = SplitExternalHosts(value)
		}

		if key == DockerRouterLabelPort {
			if data.port != 0 {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelPort)
				return
			}
			var err error
			data.port, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
					WithError(err).
					Warnf("ignoring container with invalid %s label", DockerRouterLabelPort)
				return
			}
		}
		if key == DockerRouterLabelDefault {
			if data.def != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelDefault)
				return
			}
			data.def = new(bool)

			lowerValue := strings.TrimSpace(strings.ToLower(value))
			*data.def = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
		}
		if key == DockerRouterLabelNetwork {
			if data.network != nil {
				logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
					Warnf("ignoring container with duplicate %s label", DockerRouterLabelNetwork)
				return
			}
			data.network = new(string)
			*data.network = value
		}
	}

	// probably not minecraft related
	if len(data.hosts) == 0 {
		return
	}

	if len(container.NetworkSettings.Networks) == 0 {
		logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
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
			logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
				Warnf("ignoring container, multiple networks found and none specified using label %s", DockerRouterLabelNetwork)
			return
		}

		for _, endpoint := range container.NetworkSettings.Networks {
			data.ip = endpoint.IPAddress
			break
		}
	}

	if data.ip == "" {
		logrus.WithFields(logrus.Fields{"containerId": container.ID, "containerNames": container.Names}).
			Warnf("ignoring container, unable to find accessible ip address")
		return
	}

	ok = true

	return
}

type routableContainer struct {
	externalContainerName string
	containerEndpoint     string
}
