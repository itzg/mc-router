package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"syscall"
	"time"

	"github.com/itzg/go-flagsfiller"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
)

type MetricsBackendConfig struct {
	Influxdb struct {
		Interval        time.Duration     `default:"1m"`
		Tags            map[string]string `usage:"any extra tags to be included with all reported metrics"`
		Addr            string
		Username        string
		Password        string
		Database        string
		RetentionPolicy string
	}
}

type Config struct {
	Port                  int               `default:"25565" usage:"The [port] bound to listen for Minecraft client connections"`
	Default               string            `usage:"host:port of a default Minecraft server to use when mapping not found"`
	Mapping               map[string]string `usage:"Comma or newline delimited or repeated mappings of externalHostname=host:port"`
	ApiBinding            string            `usage:"The [host:port] bound for servicing API requests"`
	Version               bool              `usage:"Output version and exit"`
	CpuProfile            string            `usage:"Enables CPU profiling and writes to given path"`
	Debug                 bool              `usage:"Enable debug logs"`
	ConnectionRateLimit   int               `default:"1" usage:"Max number of connections to allow per second"`
	InKubeCluster         bool              `usage:"Use in-cluster Kubernetes config"`
	KubeConfig            string            `usage:"The path to a Kubernetes configuration file"`
	AutoScaleUp           bool              `usage:"Increase Kubernetes StatefulSet Replicas (only) from 0 to 1 on respective backend servers when accessed"`
	InDocker              bool              `usage:"Use Docker service discovery"`
	InDockerSwarm         bool              `usage:"Use Docker Swarm service discovery"`
	DockerSocket          string            `default:"unix:///var/run/docker.sock" usage:"Path to Docker socket to use"`
	DockerTimeout         int               `default:"0" usage:"Timeout configuration in seconds for the Docker integrations"`
	DockerRefreshInterval int               `default:"15" usage:"Refresh interval in seconds for the Docker integrations"`
	MetricsBackend        string            `default:"discard" usage:"Backend to use for metrics exposure/publishing: discard,expvar,influxdb,prometheus"`
	UseProxyProtocol      bool              `default:"false" usage:"Send PROXY protocol to backend servers"`
	ReceiveProxyProtocol  bool              `default:"false" usage:"Receive PROXY protocol from backend servers, by default trusts every proxy header that it receives, combine with -trusted-proxies to specify a list of trusted proxies"`
	TrustedProxies        []string          `usage:"Comma delimited list of CIDR notation IP blocks to trust when receiving PROXY protocol"`
	MetricsBackendConfig  MetricsBackendConfig
	RoutesConfig          string `usage:"Name or full path to routes config file"`
	NgrokToken            string `usage:"If set, an ngrok tunnel will be established. It is HIGHLY recommended to pass as an environment variable."`

	ClientsToAllow []string `usage:"Zero or more client IP addresses or CIDRs to allow. Takes precedence over deny."`
	ClientsToDeny  []string `usage:"Zero or more client IP addresses or CIDRs to deny. Ignored if any configured to allow"`

	SimplifySRV bool `default:"false" usage:"Simplify fully qualified SRV records for mapping"`
}

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func showVersion() {
	fmt.Printf("%v, commit %v, built at %v", version, commit, date)
}

func main() {
	var config Config
	err := flagsfiller.Parse(&config, flagsfiller.WithEnv(""))
	if err != nil {
		logrus.Fatal(err)
	}

	if config.Version {
		showVersion()
		os.Exit(0)
	}

	if config.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debug("Debug logs enabled")
	}

	if config.CpuProfile != "" {
		cpuProfileFile, err := os.Create(config.CpuProfile)
		if err != nil {
			logrus.WithError(err).Fatal("trying to create cpu profile file")
		}
		//goland:noinspection GoUnhandledErrorResult
		defer cpuProfileFile.Close()

		logrus.WithField("file", config.CpuProfile).Info("Starting cpu profiling")
		err = pprof.StartCPUProfile(cpuProfileFile)
		if err != nil {
			logrus.WithError(err).Fatal("trying to start cpu profile")
		}
		defer pprof.StopCPUProfile()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metricsBuilder := NewMetricsBuilder(config.MetricsBackend, &config.MetricsBackendConfig)

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	if config.RoutesConfig != "" {
		err := server.RoutesConfig.ReadRoutesConfig(config.RoutesConfig)
		if err != nil {
			logrus.WithError(err).Error("Unable to load routes from config file")
		}
	}

	server.Routes.RegisterAll(config.Mapping)
	if config.Default != "" {
		server.Routes.SetDefaultRoute(config.Default)
	}

	if config.ConnectionRateLimit < 1 {
		config.ConnectionRateLimit = 1
	}

	trustedIpNets := make([]*net.IPNet, 0)
	for _, ip := range config.TrustedProxies {
		_, ipNet, err := net.ParseCIDR(ip)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to parse trusted proxy CIDR block")
		}
		trustedIpNets = append(trustedIpNets, ipNet)
	}

	connector := server.NewConnector(metricsBuilder.BuildConnectorMetrics(), config.UseProxyProtocol, config.ReceiveProxyProtocol, trustedIpNets)

	clientFilter, err := server.NewClientFilter(config.ClientsToAllow, config.ClientsToDeny)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to create client filter")
	}
	connector.SetClientFilter(clientFilter)

	if config.NgrokToken != "" {
		connector.UseNgrok(config.NgrokToken)
	}
	err = connector.StartAcceptingConnections(ctx,
		net.JoinHostPort("", strconv.Itoa(config.Port)),
		config.ConnectionRateLimit,
	)
	if err != nil {
		logrus.Fatal(err)
	}

	if config.ApiBinding != "" {
		server.StartApiServer(config.ApiBinding)
	}

	if config.InKubeCluster {
		err = server.K8sWatcher.StartInCluster(config.AutoScaleUp)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	} else if config.KubeConfig != "" {
		err := server.K8sWatcher.StartWithConfig(config.KubeConfig, config.AutoScaleUp)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	}

	if config.InDocker {
		err = server.DockerWatcher.Start(config.DockerSocket, config.DockerTimeout, config.DockerRefreshInterval)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start docker integration")
		} else {
			defer server.DockerWatcher.Stop()
		}
	}

	if config.InDockerSwarm {
		err = server.DockerSwarmWatcher.Start(config.DockerSocket, config.DockerTimeout, config.DockerRefreshInterval)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start docker swarm integration")
		} else {
			defer server.DockerSwarmWatcher.Stop()
		}
	}

	server.Routes.SimplifySRV(config.SimplifySRV)

	err = metricsBuilder.Start(ctx)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to start metrics reporter")
	}

	// wait for process-stop signal
	<-c
	logrus.Info("Stopping. Waiting for connections to complete...")
	signal.Stop(c)
	connector.WaitForConnections()
	logrus.Info("Stopped")
}
