package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/itzg/go-flagsfiller"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
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
	Trace   bool `usage:"Enable trace logs"`

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

	if cliConfig.Trace {
		logrus.SetLevel(logrus.TraceLevel)
		logrus.Trace("Trace logs enabled")
	} else if cliConfig.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debug("Debug logs enabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)

	s, err := server.NewServer(ctx, &cliConfig.ServerConfig)
	if err != nil {
		logrus.WithError(err).Fatal("Could not setup server")
	}

	var wg sync.WaitGroup
	wg.Go(s.Run)

signalsLoop:
	for {
		select {
		case <-ctx.Done():
			break signalsLoop

		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				s.ReloadConfig()

			default:
				logrus.WithField("signal", sig).Warn("Received unexpected signal")
			}
		}
	}

	logrus.Info("Stopping")
	wg.Wait()
}
