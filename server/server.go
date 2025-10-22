package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
)

type Server struct {
	ctx              context.Context
	config           *Config
	connector        *Connector
	reloadConfigChan chan struct{}
	doneChan         chan struct{}
}

func NewServer(ctx context.Context, config *Config) (*Server, error) {
	if config.CpuProfile != "" {
		cpuProfileFile, err := os.Create(config.CpuProfile)
		if err != nil {
			return nil, fmt.Errorf("could not create cpu profile file: %w", err)
		}
		//goland:noinspection GoUnhandledErrorResult
		defer cpuProfileFile.Close()

		logrus.WithField("file", config.CpuProfile).Info("Starting cpu profiling")
		err = pprof.StartCPUProfile(cpuProfileFile)
		if err != nil {
			return nil, fmt.Errorf("could not start cpu profile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}

	var err error

	var autoScaleAllowDenyConfig *AllowDenyConfig = nil
	if config.AutoScale.AllowDeny != "" {
		autoScaleAllowDenyConfig, err = ParseAllowDenyConfig(config.AutoScale.AllowDeny)
		if err != nil {
			return nil, fmt.Errorf("could not parse autoscale allow-deny-list: %w", err)
		}
	}

	metricsBuilder := NewMetricsBuilder(config.MetricsBackend, &config.MetricsBackendConfig)

	downScalerEnabled := config.AutoScale.Down && (config.InKubeCluster || config.KubeConfig != "")
	downScalerDelay, err := time.ParseDuration(config.AutoScale.DownAfter)
	if err != nil {
		return nil, fmt.Errorf("could not parse auto-scale-down-after duration: %w", err)
	}
	// Only one instance should be created
	DownScaler = NewDownScaler(ctx, downScalerEnabled, downScalerDelay)

	if config.Routes.Config != "" {
		err := RoutesConfigLoader.Load(config.Routes.Config)
		if err != nil {
			return nil, fmt.Errorf("could not load routes config file: %w", err)
		}

		if config.Routes.ConfigWatch {
			err := RoutesConfigLoader.WatchForChanges(ctx)
			if err != nil {
				return nil, fmt.Errorf("could not watch for changes to routes config file: %w", err)
			}
		}
	}

	Routes.RegisterAll(config.Mapping)
	if config.Default != "" {
		Routes.SetDefaultRoute(config.Default)
	}

	if config.ConnectionRateLimit < 1 {
		config.ConnectionRateLimit = 1
	}

	connector := NewConnector(ctx,
		metricsBuilder.BuildConnectorMetrics(),
		config.UseProxyProtocol,
		config.RecordLogins,
		autoScaleAllowDenyConfig)

	clientFilter, err := NewClientFilter(config.ClientsToAllow, config.ClientsToDeny)
	if err != nil {
		return nil, fmt.Errorf("could not create client filter: %w", err)
	}
	connector.UseClientFilter(clientFilter)

	if config.Webhook.Url != "" {
		logrus.WithField("url", config.Webhook.Url).
			WithField("require-user", config.Webhook.RequireUser).
			Info("Using webhook for connection status notifications")
		connector.UseConnectionNotifier(
			NewWebhookNotifier(config.Webhook.Url, config.Webhook.RequireUser))
	}

	if config.Ngrok.Token != "" {
		connector.UseNgrok(config.Ngrok)
	}

	if config.ReceiveProxyProtocol {
		trustedIpNets := make([]*net.IPNet, 0)
		for _, ip := range config.TrustedProxies {
			_, ipNet, err := net.ParseCIDR(ip)
			if err != nil {
				return nil, fmt.Errorf("could not parse trusted proxy CIDR block: %w", err)
			}
			trustedIpNets = append(trustedIpNets, ipNet)
		}

		connector.UseReceiveProxyProto(trustedIpNets)
	}

	if config.ApiBinding != "" {
		StartApiServer(config.ApiBinding)
	}

	routeWatchers := make([]RouteFinder, 0)

	if config.InKubeCluster {
		k8sWatcher, err := NewK8sWatcherInCluster()
		if err != nil {
			return nil, fmt.Errorf("could not create in-cluster k8s watcher: %w", err)
		}
		k8sWatcher.WithAutoScale(config.AutoScale.Up, config.AutoScale.Down)
		k8sWatcher.WithNamespace(config.KubeNamespace)
		routeWatchers = append(routeWatchers, k8sWatcher)
	} else if config.KubeConfig != "" {
		k8sWatcher, err := NewK8sWatcherWithConfig(config.KubeConfig)
		if err != nil {
			return nil, fmt.Errorf("could not create k8s watcher with kube config: %w", err)
		}
		k8sWatcher.WithAutoScale(config.AutoScale.Up, config.AutoScale.Down)
		k8sWatcher.WithNamespace(config.KubeNamespace)
		routeWatchers = append(routeWatchers, k8sWatcher)
	}

	// TODO convert to RouteFinder
	if config.InDocker {
		err = DockerWatcher.Start(ctx, config.DockerSocket, config.DockerTimeout, config.DockerRefreshInterval, config.AutoScale.Up, config.AutoScale.Down)
		if err != nil {
			return nil, fmt.Errorf("could not start docker integration: %w", err)
		}
	}

	// TODO convert to RouteFinder
	if config.InDockerSwarm {
		err = DockerSwarmWatcher.Start(ctx, config.DockerSocket, config.DockerTimeout, config.DockerRefreshInterval, config.AutoScale.Up, config.AutoScale.Down)
		if err != nil {
			return nil, fmt.Errorf("could not start docker swarm integration: %w", err)
		}
	}

	for _, watcher := range routeWatchers {
		err := watcher.Start(ctx, Routes)
		if err != nil {
			return nil, fmt.Errorf("could not start route watcher %s: %w", watcher, err)
		}
	}

	Routes.SimplifySRV(config.SimplifySRV)

	err = metricsBuilder.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not start metrics reporter: %w", err)
	}

	return &Server{
		ctx:              ctx,
		config:           config,
		connector:        connector,
		reloadConfigChan: make(chan struct{}),
		doneChan:         make(chan struct{}),
	}, nil
}

// ReloadConfig indicates that an external request, such as a SIGHUP,
// is requesting the routes config file to be reloaded, if enabled
func (s *Server) ReloadConfig() {
	s.reloadConfigChan <- struct{}{}
}

// AcceptConnection provides a way to externally supply a connection to consume
// Note that this will skip rate limiting.
func (s *Server) AcceptConnection(conn net.Conn) {
	s.connector.AcceptConnection(conn)
}

// Run will run the server until the context is done or a fatal error occurs, so this should be
// in a go routine.
func (s *Server) Run() {
	err := s.connector.StartAcceptingConnections(
		net.JoinHostPort("", strconv.Itoa(s.config.Port)),
		s.config.ConnectionRateLimit,
	)
	if err != nil {
		logrus.WithError(err).Error("Could not start accepting connections")
		return
	}

	for {
		select {
		case <-s.reloadConfigChan:
			if err := RoutesConfigLoader.Reload(); err != nil {
				logrus.WithError(err).
					Error("Could not re-read the routes config file")
			}

		case <-s.ctx.Done():
			logrus.Info("Server Stopping. Waiting for connections to complete...")
			s.connector.WaitForConnections()
			logrus.Info("Stopped")
			return
		}
	}

}
