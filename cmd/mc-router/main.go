package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
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
	Port                 int      `default:"25565" usage:"The [port] bound to listen for Minecraft client connections"`
	Mapping              []string `usage:"Comma-separated or repeated mappings of externalHostname=host:port"`
	ApiBinding           string   `usage:"The [host:port] bound for servicing API requests"`
	Version              bool     `usage:"Output version and exit"`
	CpuProfile           string   `usage:"Enables CPU profiling and writes to given path"`
	Debug                bool     `usage:"Enable debug logs"`
	ConnectionRateLimit  int      `default:"1" usage:"Max number of connections to allow per second"`
	InKubeCluster        bool     `usage:"Use in-cluster kubernetes config"`
	KubeConfig           string   `usage:"The path to a kubernetes configuration file"`
	MetricsBackend       string   `default:"discard" usage:"Backend to use for metrics exposure/publishing: discard,expvar,influxdb"`
	UseProxyProtocol     bool     `default:"false" usage:"Send PROXY protocol to backend servers"`
	MetricsBackendConfig MetricsBackendConfig
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

	server.Routes.RegisterAll(parseMappings(config.Mapping))

	if config.ConnectionRateLimit < 1 {
		config.ConnectionRateLimit = 1
	}
	connector := server.NewConnector(metricsBuilder.BuildConnectorMetrics(), config.UseProxyProtocol)
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
		err = server.K8sWatcher.StartInCluster()
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	} else if config.KubeConfig != "" {
		err := server.K8sWatcher.StartWithConfig(config.KubeConfig)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	}

	err = metricsBuilder.Start(ctx)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to start metrics reporter")
	}

	// wait for process-stop signal
	<-c
	logrus.Info("Stopping")
}

func parseMappings(vals []string) map[string]string {
	result := make(map[string]string)
	for _, part := range vals {
		keyValue := strings.Split(part, "=")
		if len(keyValue) == 2 {
			result[keyValue[0]] = keyValue[1]
		} else {
			logrus.WithField("part", part).Fatal("Invalid part of mapping")
		}
	}

	return result
}
