package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
)

var (
	port           = flag.Int("port", 25565, "The port bound to listen for Minecraft client connections")
	apiBinding     = flag.String("api-binding", "", "The host:port bound for servicing API requests")
	mappings       = flag.String("mapping", "", "Comma-separated mappings of externalHostname=host:port")
	versionFlag    = flag.Bool("version", false, "Output version and exit")
	kubeConfigFile = flag.String("kube-config", "", "The path to a kubernetes configuration file")
	inKubeCluster  = flag.Bool("in-kube-cluster", false, "Use in-cluster kubernetes config")
	cpuProfile     = flag.String("cpu-profile", "", "Enables CPU profiling and writes to given path")
	debug          = flag.Bool("debug", false, "Enable debug logs")
	connRateLimit  = flag.Int("connection-rate-limit", 1, "Max number of connections to allow per second")
	metricsBackend = flag.String("metrics-backend", "discard", "Backend to use for metrics exposure/publishing: discard,expvar")
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func showVersion() {
	fmt.Printf("%v, commit %v, built at %v", version, commit, date)
}

func main() {
	flag.Parse()

	if *versionFlag {
		showVersion()
		os.Exit(0)
	}

	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debug("Debug logs enabled")
	}

	if *cpuProfile != "" {
		cpuProfileFile, err := os.Create(*cpuProfile)
		if err != nil {
			logrus.WithError(err).Fatal("trying to create cpu profile file")
		}
		defer cpuProfileFile.Close()

		logrus.WithField("file", *cpuProfile).Info("Starting cpu profiling")
		err = pprof.StartCPUProfile(cpuProfileFile)
		if err != nil {
			logrus.WithError(err).Fatal("trying to start cpu profile")
		}
		defer pprof.StopCPUProfile()
	}

	ctx, cancel := context.WithCancel(context.Background())

	metricsBuilder := NewMetricsBuilder()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	server.Routes.RegisterAll(parseMappings(*mappings))

	if *connRateLimit < 1 {
		*connRateLimit = 1
	}
	connector := server.NewConnector(metricsBuilder.BuildConnectorMetrics())
	connector.StartAcceptingConnections(ctx, net.JoinHostPort("", strconv.Itoa(*port)), *connRateLimit)

	if *apiBinding != "" {
		server.StartApiServer(*apiBinding)
	}

	var err error
	if *inKubeCluster {
		err = server.K8sWatcher.StartInCluster()
		if err != nil {
			logrus.WithError(err).Warn("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	} else if *kubeConfigFile != "" {
		err := server.K8sWatcher.StartWithConfig(*kubeConfigFile)
		if err != nil {
			logrus.WithError(err).Warn("Unable to start k8s integration")
		} else {
			defer server.K8sWatcher.Stop()
		}
	}

	<-c
	logrus.Info("Stopping")
	cancel()
}

func parseMappings(val string) map[string]string {
	result := make(map[string]string)
	if val != "" {
		parts := strings.Split(val, ",")
		for _, part := range parts {
			keyValue := strings.Split(part, "=")
			if len(keyValue) == 2 {
				result[keyValue[0]] = keyValue[1]
			} else {
				logrus.WithField("part", part).Fatal("Invalid part of mapping")
			}
		}
	}

	return result
}
