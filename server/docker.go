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
	DockerConfigHost = "unix:///var/run/docker.sock"
	DockerAPIVersion = "1.24"
)

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
		Routes.CreateMapping(s.externalServiceName, s.containerEndpoint, func(ctx context.Context) error {
			return nil
		})
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
						Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, func(ctx context.Context) error {
							return nil
						})
						logrus.WithField("routableService", rs).Debug("ADD")
					} else if oldRs.containerEndpoint != rs.containerEndpoint {
						serviceMap[rs.externalServiceName] = rs
						Routes.DeleteMapping(rs.externalServiceName)
						Routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, func(ctx context.Context) error {
							return nil
						})
						logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
					}
					visited[rs.externalServiceName] = struct{}{}
				}
				for _, rs := range serviceMap {
					if _, ok := visited[rs.externalServiceName]; !ok {
						delete(serviceMap, rs.externalServiceName)
						Routes.DeleteMapping(rs.externalServiceName)
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

		var port uint64 = 25565
		var hosts []string
		for key, value := range service.Spec.Labels {
			if key == "mc-router.host" {
				hosts = strings.Split(value, ",")
			}
			if key == "mc-router.port" {
				port, err = strconv.ParseUint(value, 10, 32)
				if err != nil {
					// TODO: report?
					continue
				}
			}
		}
		if len(hosts) == 0 {
			continue
		}

		virtualIP := service.Endpoint.VirtualIPs[0]
		ip, _, _ := net.ParseCIDR(virtualIP.Addr)

		for _, host := range hosts {
			result = append(result, &routableService{
				containerEndpoint:   fmt.Sprintf("%s:%d", ip.String(), port),
				externalServiceName: host,
			})
		}
	}

	return result, nil
}

func (w *dockerWatcherImpl) Stop() {
	if w.contextCancel != nil {
		w.contextCancel()
	}
}
