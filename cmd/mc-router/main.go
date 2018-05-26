package main

import (
	"context"
	"fmt"
	"github.com/alecthomas/kingpin"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
	"net"
	"os"
	"os/signal"
	"strconv"
)

var (
	port = kingpin.Flag("port", "The port bound to listen for Minecraft client connections").
		Default("25565").Int()
	apiBinding = kingpin.Flag("api-binding", "The host:port bound for servicing API requests").
			String()
	mappings = kingpin.Flag("mapping", "Mapping of external hostname to internal server host:port").
			StringMap()
	versionFlag = kingpin.Flag("version", "Output version and exit").
			Bool()
	kubeConfigFile = kingpin.Flag("kube-config", "The path to a kubernetes configuration file").String()
	inKubeCluster  = kingpin.Flag("in-kube-cluster", "Use in-cluster kubernetes config").Bool()
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
	kingpin.Parse()

	if *versionFlag {
		showVersion()
		os.Exit(0)
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	server.Routes.RegisterAll(*mappings)

	server.Connector.StartAcceptingConnections(ctx, net.JoinHostPort("", strconv.Itoa(*port)))

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
