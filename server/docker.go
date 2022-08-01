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
	"github.com/docker/docker/api/types/swarm"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type IDockerWatcher interface {
	StartInSwarm(timeoutSeconds int, refreshIntervalSeconds int) error
	Stop()
}

var DockerWatcher IDockerWatcher = &dockerWatcherImpl{}

type dockerWatcherImpl struct {
	sync.RWMutex
	client        *client.Client
	contextCancel context.CancelFunc
}

const (
	DockerConfigHost         = "unix:///var/run/docker.sock"
	DockerAPIVersion         = "1.24"
	DockerRouterLabelHost    = "mc-router.host"
	DockerRouterLabelPort    = "mc-router.port"
	DockerRouterLabelDefault = "mc-router.default"
)

func (w *dockerWatcherImpl) makeWakerFunc(service *routableService) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return nil
	}
}

func (w *dockerWatcherImpl) StartInSwarm(timeoutSeconds int, refreshIntervalSeconds int) error {
	var err error

	timeout := time.Duration(timeoutSeconds) * time.Second
	refreshInterval := time.Duration(refreshIntervalSeconds) * time.Second

	opts := []client.Opt{
		client.WithHost(DockerConfigHost),
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

	logrus.Info("Monitoring Docker for Minecraft services")
	return nil
}

func (w *dockerWatcherImpl) listServices(ctx context.Context) ([]*routableService, error) {
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

	networkList, err := w.client.NetworkList(ctx, dockertypes.NetworkListOptions{Filters: networkListArgs})
	if err != nil {
		return nil, err
	}

	networkMap := make(map[string]*dockertypes.NetworkResource)
	for _, network := range networkList {
		networkToAdd := network
		networkMap[network.ID] = &networkToAdd
	}

	var result []*routableService
	for _, service := range services {
		if service.Spec.EndpointSpec.Mode != swarmtypes.ResolutionModeVIP {
			continue
		}
		if len(service.Endpoint.VirtualIPs) == 0 {
			continue
		}

		data, ok := w.parseServiceData(&service)
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

type parsedDockerServiceData struct {
	hosts []string
	port  uint64
	def   *bool
	ip    string
}

func (w *dockerWatcherImpl) parseServiceData(service *swarm.Service) (data parsedDockerServiceData, ok bool) {
	ok = true
	for key, value := range service.Spec.Labels {
		if key == DockerRouterLabelHost {
			if data.hosts != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelHost)
				ok = false
				return
			}
			data.hosts = strings.Split(value, ",")
		}
		if key == DockerRouterLabelPort {
			if data.port != 0 {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelPort)
				ok = false
				return
			}
			var err error
			data.port, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid %s", DockerRouterLabelPort)
				ok = false
				return
			}
		}
		if key == DockerRouterLabelDefault {
			if data.def != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelDefault)
				ok = false
				return
			}
			data.def = new(bool)

			lowerValue := strings.TrimSpace(strings.ToLower(value))
			*data.def = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"
		}
	}
	if len(data.hosts) == 0 {
		ok = false
		return
	}
	if data.port == 0 {
		data.port = 25565
	}

	virtualIP := service.Endpoint.VirtualIPs[0]
	ip, _, _ := net.ParseCIDR(virtualIP.Addr)
	data.ip = ip.String()

	return
}

func (w *dockerWatcherImpl) Stop() {
	if w.contextCancel != nil {
		w.contextCancel()
	}
}
