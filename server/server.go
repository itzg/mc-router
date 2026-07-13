package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime/pprof"
	"strconv"

	"github.com/sirupsen/logrus"
)

type Server struct {
	ctx                context.Context
	config             *Config
	connector          *Connector
	reloadConfigChan   chan struct{}
	doneChan           chan struct{}
	routesConfigLoader *RoutesConfigLoader
	routes             IRoutes
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

	routes := NewRoutes(ctx)

	webhookScalerConfigured := config.AutoScale.Webhook.Url != ""
	downScalerEnabled := (config.AutoScale.Down && (config.InKubeCluster || config.KubeConfig != "" || config.InDocker)) || webhookScalerConfigured
	downScalerDelay := config.AutoScale.DownAfter
	// Only one instance should be created
	// TODO why create it if not enabled? nil checks needed if optional
	downscaler := NewDownScaler(downScalerEnabled, downScalerDelay)
	routes.WithDownScaler(downscaler)

	// Build the webhook scaler and hand it to the objects that register static
	// routes so they pick up its waker/sleeper. Discovery-based routes
	// (Docker/Kubernetes) supply their own and are unaffected. A nil scaler
	// (unconfigured) is fine: routeFuncs is nil-safe.
	var webhookScaler *WebhookScaler
	if webhookScalerConfigured {
		webhookScaler = NewWebhookScaler(
			config.AutoScale.Webhook.Url,
			config.AutoScale.Webhook.Headers,
			config.AutoScale.Webhook.Timeout,
			config.AutoScale.Webhook.WakeTimeout,
		)
		logrus.WithField("url", config.AutoScale.Webhook.Url).
			Info("Using webhook autoscaler for static routes")
	}

	var routesConfigLoader *RoutesConfigLoader
	if config.Routes.Config != "" {
		routesConfigLoader = NewRoutesConfigLoader(webhookScaler, routes)
		err := routesConfigLoader.Load(config.Routes.Config)
		if err != nil {
			return nil, fmt.Errorf("could not load routes config file: %w", err)
		}

		if config.Routes.ConfigWatch {
			err := routesConfigLoader.WatchForChanges(ctx)
			if err != nil {
				return nil, fmt.Errorf("could not watch for changes to routes config file: %w", err)
			}
		}
	}

	routes.BulkRegister(webhookScaler, config.Mapping)
	if config.Default != "" {
		waker, sleeper := webhookScaler.routeFuncs("", config.Default)
		routes.SetDefaultRoute(config.Default, "", waker, sleeper, "", "")
	}

	if config.ConnectionRateLimit < 1 {
		config.ConnectionRateLimit = 1
	}

	connector := NewConnector(ctx, routes, downscaler, metricsBuilder.BuildConnectorMetrics(), config.UseProxyProtocol, config.RecordLogins, autoScaleAllowDenyConfig)

	connector.UseBackendDialTimeout(config.BackendDialTimeout)
	connector.UseAsleepMOTD(config.AutoScale.AsleepMOTD)
	connector.UseLoadingMOTD(config.AutoScale.LoadingMOTD)

	clientFilter, err := NewClientFilter(config.ClientsToAllow, config.ClientsToDeny)
	if err != nil {
		return nil, fmt.Errorf("could not create client filter: %w", err)
	}
	connector.UseClientFilter(clientFilter)

	if config.Webhook.Url != "" {
		logrus.WithField("url", config.Webhook.Url).
			WithField("require-user", config.Webhook.RequireUser).
			Info("Using webhook for connection status notifications")
		notifier := NewWebhookNotifier(config.Webhook.Url, config.Webhook.RequireUser, config.Webhook.Timeout, config.Webhook.Events)
		connector.UseConnectionNotifier(notifier)
		routes.WithListener(notifier)
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
		StartApiServer(config.ApiBinding, routes, routesConfigLoader, webhookScaler)
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

	if config.DockerRefreshInterval != 0 {
		logrus.WithField("value", config.DockerRefreshInterval).
			Warn("--docker-refresh-interval is deprecated and ignored; Docker discovery is now event-driven")
	}

	// TODO convert to RouteFinder
	if config.InDocker {
		watcher := NewDockerWatcher(config.DockerSocket, config.DockerTimeout, config.AutoScale.Up, config.AutoScale.Down, config.DockerApiVersion, routes)
		err = watcher.Start(ctx)
		if err != nil {
			return nil, fmt.Errorf("could not start docker integration: %w", err)
		}
	}

	// TODO convert to RouteFinder
	if config.InDockerSwarm {
		watcher := NewDockerSwarmWatcher(config.DockerSocket, config.DockerTimeout, config.AutoScale.Up, config.AutoScale.Down, config.DockerApiVersion, routes)
		err = watcher.Start(ctx)
		if err != nil {
			return nil, fmt.Errorf("could not start docker swarm integration: %w", err)
		}
	}

	for _, watcher := range routeWatchers {
		err := watcher.Start(ctx, routes)
		if err != nil {
			return nil, fmt.Errorf("could not start route watcher %s: %w", watcher, err)
		}
	}

	routes.SimplifySRV(config.SimplifySRV)

	err = metricsBuilder.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not start metrics reporter: %w", err)
	}

	return &Server{
		ctx:                ctx,
		config:             config,
		connector:          connector,
		routes:             routes,
		routesConfigLoader: routesConfigLoader,
		reloadConfigChan:   make(chan struct{}),
		doneChan:           make(chan struct{}),
	}, nil
}

// ReloadConfig indicates that an external request, such as a SIGHUP,
// is requesting the routes config file to be reloaded, if enabled
func (s *Server) ReloadConfig() {
	s.reloadConfigChan <- struct{}{}
}

// AcceptConnection provides a way to externally supply a connection to consume
// Notes:
// - this will bypass rate limiting
// - this function returns immediately by starting its own go routine to handle the connection
func (s *Server) AcceptConnection(conn net.Conn) {
	logrus.
		WithField("remoteAddr", conn.RemoteAddr()).
		Debug("Accepting connection from external source")
	s.connector.AcceptConnection(conn)
}

// Run will run the server until the context is done or a fatal error occurs, so this should be
// in a go routine.
func (s *Server) Run() {
	err := s.connector.StartAcceptingConnections(
		net.JoinHostPort("", strconv.Itoa(s.config.Port)),
		s.config.ConnectionRateLimit,
		s.config.MetricsRateLimitPeriod,
	)
	if err != nil {
		logrus.WithError(err).Error("Could not start accepting connections")
		return
	}

	for {
		select {
		case <-s.reloadConfigChan:
			if err := s.routesConfigLoader.Reload(); err != nil {
				logrus.WithError(err).
					Error("Could not re-read the routes config file")
			}

		case <-s.ctx.Done():
			logrus.Info("Router server stopping. Waiting for connections to complete...")
			s.connector.WaitForConnections()
			logrus.Info("Router server stopped")
			return
		}
	}

}
