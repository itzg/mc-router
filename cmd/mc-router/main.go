package main

import (
	"context"
	"fmt"
	"github.com/itzg/go-flagsfiller"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"syscall"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func showVersion() {
	fmt.Printf("%v, commit %v, built at %v", version, commit, date)
}

type CliConfig struct {
	Version bool `usage:"Output version and exit"`
	Debug   bool `usage:"Enable debug logs"`

	ServerConfig server.Config `flatten:"true"`
}

func main() {
	var cliConfig CliConfig
	err := flagsfiller.Parse(&cliConfig, flagsfiller.WithEnv(""))
	if err != nil {
		logrus.Fatal(err)
	}

	if cliConfig.Version {
		showVersion()
		os.Exit(0)
	}

	if cliConfig.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debug("Debug logs enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	s, err := server.NewServer(ctx, &cliConfig.ServerConfig)
	if err != nil {
		logrus.WithError(err).Fatal("Could not setup server")
	}

	go s.Run()

	for {
		select {
		case <-s.Done():
			return

		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				s.ReloadConfig()

			case syscall.SIGINT, syscall.SIGTERM:
				cancel()
				// but wait for the server to be done

			default:
				logrus.WithField("signal", sig).Warn("Received unexpected signal")
			}
		}
	}
}
