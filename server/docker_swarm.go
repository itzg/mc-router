package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

func NewDockerSwarmWatcher(socket string, timeout time.Duration, autoScaleUp bool, autoScaleDown bool, dockerApiVersion string) IDockerWatcher {
	return &dockerSwarmWatcherImpl{
		config: dockerWatcherConfig{
			socket:        socket,
			timeout:       timeout,
			autoScaleUp:   autoScaleUp,
			autoScaleDown: autoScaleDown,
			apiVersion:    dockerApiVersion,
		},
	}
}

type dockerSwarmWatcherImpl struct {
	sync.RWMutex
	config      dockerWatcherConfig
	client      *client.Client
	serviceMap  map[string]*routableService
	monitorLock sync.Mutex
}

func (w *dockerSwarmWatcherImpl) makeWakerFunc(_ *routableService) WakerFunc {
	if !w.config.autoScaleUp {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		logrus.Fatal("Auto scale up is not yet supported for docker swarm")
		return "", nil
	}
}

func (w *dockerSwarmWatcherImpl) makeSleeperFunc(_ *routableService) SleeperFunc {
	if !w.config.autoScaleDown {
		return nil
	}
	return func(ctx context.Context) error {
		logrus.Fatal("Auto scale down is not yet supported for docker swarm")
		return nil
	}
}

func (w *dockerSwarmWatcherImpl) Start(ctx context.Context) error {
	var err error

	opts := []client.Opt{
		client.WithHost(w.config.socket),
		client.WithTimeout(w.config.timeout),
		client.WithHTTPHeaders(map[string]string{
			"User-Agent": "mc-router ",
		}),
		client.WithAPIVersionNegotiation(),
	}

	w.client, err = client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}

	w.serviceMap = map[string]*routableService{}

	logrus.Trace("Performing initial listing of Docker swarm services")
	if err := w.reconcileServices(ctx); err != nil {
		return err
	}

	go w.streamEvents(ctx)

	logrus.Info("Monitoring Docker Swarm for Minecraft services")
	return nil
}

func (w *dockerSwarmWatcherImpl) reconcileServices(ctx context.Context) error {
	w.monitorLock.Lock()
	defer w.monitorLock.Unlock()

	services, err := w.listServices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Docker failed to list services")
		return err
	}

	visited := map[string]struct{}{}
	for _, rs := range services {
		if oldRs, ok := w.serviceMap[rs.externalServiceName]; !ok {
			w.serviceMap[rs.externalServiceName] = rs
			logrus.WithField("routableService", rs).Debug("ADD")
			wakerFunc := w.makeWakerFunc(rs)
			sleeperFunc := w.makeSleeperFunc(rs)
			if rs.externalServiceName != "" {
				Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, "", wakerFunc, sleeperFunc, "", "")
			} else {
				Routes.SetDefaultRoute(rs.containerEndpoint, "", wakerFunc, sleeperFunc, "", "")
			}
		} else if oldRs.containerEndpoint != rs.containerEndpoint {
			w.serviceMap[rs.externalServiceName] = rs
			wakerFunc := w.makeWakerFunc(rs)
			sleeperFunc := w.makeSleeperFunc(rs)
			if rs.externalServiceName != "" {
				Routes.DeleteMapping(rs.externalServiceName)
				Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, "", wakerFunc, sleeperFunc, "", "")
			} else {
				Routes.SetDefaultRoute(rs.containerEndpoint, "", wakerFunc, sleeperFunc, "", "")
			}
			logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
		}
		visited[rs.externalServiceName] = struct{}{}
	}
	for _, rs := range w.serviceMap {
		if _, ok := visited[rs.externalServiceName]; !ok {
			delete(w.serviceMap, rs.externalServiceName)
			if rs.externalServiceName != "" {
				Routes.DeleteMapping(rs.externalServiceName)
			} else {
				Routes.SetDefaultRoute("", "", nil, nil, "", "")
			}
			logrus.WithField("routableService", rs).Debug("DELETE")
		}
	}
	return nil
}

func (w *dockerSwarmWatcherImpl) streamEvents(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			logrus.Debug("Stopping Docker Swarm monitoring")
			return
		}

		eventFilters := filters.NewArgs(
			filters.Arg("type", string(events.ServiceEventType)),
			filters.Arg("event", string(events.ActionCreate)),
			filters.Arg("event", string(events.ActionUpdate)),
			filters.Arg("event", string(events.ActionRemove)),
		)

		eventCh, errCh := w.client.Events(ctx, events.ListOptions{Filters: eventFilters})

		if err := w.reconcileServices(ctx); err != nil {
			logrus.WithError(err).Error("Docker Swarm resync failed")
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
				logrus.WithFields(logrus.Fields{"type": ev.Type, "action": ev.Action, "id": ev.Actor.ID}).Trace("Docker Swarm event")
				if err := w.reconcileServices(ctx); err != nil {
					logrus.WithError(err).Error("Docker Swarm reconciliation failed")
				}
			case err, ok := <-errCh:
				if !ok {
					break loop
				}
				if ctx.Err() != nil {
					return
				}
				logrus.WithError(err).Warn("Docker Swarm event stream error, reconnecting")
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

func (w *dockerSwarmWatcherImpl) listServices(ctx context.Context) ([]*routableService, error) {
	services, err := w.client.ServiceList(ctx, dockertypes.ServiceListOptions{})
	if err != nil {
		return nil, err
	}

	serverVersion, err := w.client.ServerVersion(ctx)
	if err != nil {
		return nil, err
	}

	networkListArgs := filters.NewArgs()
	// https://docs.docker.com/engine/api/v1.29/#tag/Network (Docker 17.06)
	if versions.GreaterThanOrEqualTo(serverVersion.APIVersion, "1.29") {
		networkListArgs.Add("scope", "swarm")
	} else {
		networkListArgs.Add("driver", "overlay")
	}

	networkList, err := w.client.NetworkList(ctx, network.ListOptions{Filters: networkListArgs})
	if err != nil {
		return nil, err
	}

	networkMap := make(map[string]*network.Inspect)
	for _, network := range networkList {
		networkToAdd := network
		networkMap[network.ID] = &networkToAdd
	}

	var result []*routableService
	for _, service := range services {
		if service.Spec.EndpointSpec == nil || service.Spec.EndpointSpec.Mode != swarmtypes.ResolutionModeVIP {
			continue
		}
		if len(service.Endpoint.VirtualIPs) == 0 {
			continue
		}

		data, ok := w.parseServiceData(&service, networkMap)
		if !ok {
			continue
		}

		for _, host := range data.hosts {
			result = append(result, &routableService{
				containerEndpoint:   fmt.Sprintf("%s:%d", data.ip, data.port),
				externalServiceName: host,
			})
		}
		if data.def != nil && *data.def {
			result = append(result, &routableService{
				containerEndpoint:   fmt.Sprintf("%s:%d", data.ip, data.port),
				externalServiceName: "",
			})
		}
	}

	return result, nil
}

func dockerCheckNetworkName(id string, name string, networkMap map[string]*network.Inspect, networkAliases map[string][]string) (bool, error) {
	// we allow to specify the id instead
	if id == name {
		return true, nil
	}
	if network := networkMap[id]; network != nil {
		if network.Name == name {
			return true, nil
		}
		aliases := networkAliases[id]
		for _, alias := range aliases {
			if alias == name {
				return true, nil
			}
		}
		return false, nil
	}

	return false, fmt.Errorf("network not found %s", id)
}

type parsedDockerServiceData struct {
	hosts   []string
	port    uint64
	def     *bool
	network *string
	ip      string
}

func (w *dockerSwarmWatcherImpl) parseServiceData(service *swarm.Service, networkMap map[string]*network.Inspect) (data parsedDockerServiceData, ok bool) {
	networkAliases := map[string][]string{}
	for _, network := range service.Spec.TaskTemplate.Networks {
		networkAliases[network.Target] = network.Aliases
	}

	for key, value := range service.Spec.Labels {
		if key == DockerRouterLabelHost {
			if data.hosts != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelHost)
				return
			}
			data.hosts = SplitExternalHosts(value)
		}
		if key == DockerRouterLabelPort {
			if data.port != 0 {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelPort)
				return
			}
			var err error
			data.port, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid %s", DockerRouterLabelPort)
				return
			}
		}
		if key == DockerRouterLabelDefault {
			if data.def != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelDefault)
				return
			}
			data.def = new(bool)

			lowerValue := strings.TrimSpace(strings.ToLower(value))
			*data.def = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
		}
		if key == DockerRouterLabelNetwork {
			if data.network != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelNetwork)
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

	if len(service.Endpoint.VirtualIPs) == 0 {
		logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
			Warnf("ignoring service, no VirtualIPs found")
		return
	}

	if data.port == 0 {
		data.port = 25565
	}

	vipIndex := -1
	if data.network != nil {
		for i, vip := range service.Endpoint.VirtualIPs {
			if ok, err := dockerCheckNetworkName(vip.NetworkID, *data.network, networkMap, networkAliases); ok {
				vipIndex = i
				break
			} else if err != nil {
				// we intentionally ignore name check errors
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Debugf("%v", err)
			}
		}
		if vipIndex == -1 {
			logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
				Warnf("ignoring service, network %s not found", *data.network)
			return
		}
	} else {
		// if network isn't specified assume it's the first one
		vipIndex = 0
	}

	virtualIP := service.Endpoint.VirtualIPs[vipIndex]
	ip, _, _ := net.ParseCIDR(virtualIP.Addr)
	data.ip = ip.String()
	ok = true
	return
}
