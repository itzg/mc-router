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
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

var DockerSwarmWatcher IDockerWatcher = &dockerSwarmWatcherImpl{}

type dockerSwarmWatcherImpl struct {
	sync.RWMutex
	client        *client.Client
	contextCancel context.CancelFunc
}

func (w *dockerSwarmWatcherImpl) makeWakerFunc(_ *routableService) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return nil
	}
}

func (w *dockerSwarmWatcherImpl) Start(socket string, timeoutSeconds int, refreshIntervalSeconds int) error {
	var err error

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
	serviceMap := map[string]*routableService{}

	var ctx context.Context
	ctx, w.contextCancel = context.WithCancel(context.Background())

	initialServices, err := w.listServices(ctx)
	if err != nil {
		return err
	}

	for _, s := range initialServices {
		serviceMap[s.externalServiceName] = s
		if s.externalServiceName != "" {
			Routes.CreateMapping(s.externalServiceName, s.containerEndpoint, w.makeWakerFunc(s))
		} else {
			Routes.SetDefaultRoute(s.containerEndpoint)
		}
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				services, err := w.listServices(ctx)
				if err != nil {
					logrus.WithError(err).Error("Docker failed to list services")
					return
				}

				visited := map[string]struct{}{}
				for _, rs := range services {
					if oldRs, ok := serviceMap[rs.externalServiceName]; !ok {
						serviceMap[rs.externalServiceName] = rs
						logrus.WithField("routableService", rs).Debug("ADD")
						if rs.externalServiceName != "" {
							Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, w.makeWakerFunc(rs))
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint)
						}
					} else if oldRs.containerEndpoint != rs.containerEndpoint {
						serviceMap[rs.externalServiceName] = rs
						if rs.externalServiceName != "" {
							Routes.DeleteMapping(rs.externalServiceName)
							Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, w.makeWakerFunc(rs))
						} else {
							Routes.SetDefaultRoute(rs.containerEndpoint)
						}
						logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
					}
					visited[rs.externalServiceName] = struct{}{}
				}
				for _, rs := range serviceMap {
					if _, ok := visited[rs.externalServiceName]; !ok {
						delete(serviceMap, rs.externalServiceName)
						if rs.externalServiceName != "" {
							Routes.DeleteMapping(rs.externalServiceName)
						} else {
							Routes.SetDefaultRoute("")
						}
						logrus.WithField("routableService", rs).Debug("DELETE")
					}
				}

			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()

	logrus.Info("Monitoring Docker Swarm for Minecraft services")
	return nil
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
			data.hosts = strings.Split(value, ",")
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

func (w *dockerSwarmWatcherImpl) Stop() {
	if w.contextCancel != nil {
		w.contextCancel()
	}
}
