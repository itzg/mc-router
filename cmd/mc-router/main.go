package main

import (
	"net"
	"github.com/itzg/mc-router/server"
	"github.com/alecthomas/kingpin"
	"strconv"
	"github.com/sirupsen/logrus"
	"context"
	"os"
	"os/signal"
)

var (
	port = kingpin.Flag("port", "The port bound to listen for Minecraft client connections").
		Default("25565").Int()
	apiBinding = kingpin.Flag("api-binding", "The host:port bound for servicing API requests").
		String()
	mappings = kingpin.Flag("mapping", "Mapping of external hostname to internal server host:port").
		StringMap()
)

func main() {
	kingpin.Parse()

	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 1)
	signal.Notify(c)

	server.Routes.RegisterAll(*mappings)

	server.Connector.StartAcceptingConnections(ctx, net.JoinHostPort("", strconv.Itoa(*port)))

	if *apiBinding != "" {
		server.StartApiServer(*apiBinding)
	}

	<-c
	logrus.Info("Stopping")
	cancel()
}
