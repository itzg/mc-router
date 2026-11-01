package main

import (
	"context"
	"fmt"
	"io"
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
	Version  bool         `usage:"Output version and exit"`
	Debug    bool         `usage:"Enable debug logs"`
	Trace    bool         `usage:"Enable trace logs"`
	LogLevel logrus.Level `usage:"Set a specific log filtering level, such as debug, info, warn, error\nIgnored when --debug or --trace is used" default:"info"`
	LogFile  string       `usage:"Also append logs to this file, for tools such as fail2ban"`

	ServerConfig server.Config `flatten:"true"`
}

// addLogFile makes logger also append to the file at path. The caller closes
// the returned file.
func addLogFile(logger *logrus.Logger, path string) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	logger.SetOutput(io.MultiWriter(logger.Out, file))
	return file, nil
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

	if cliConfig.LogFile != "" {
		logFile, err := addLogFile(logrus.StandardLogger(), cliConfig.LogFile)
		if err != nil {
			logrus.WithError(err).Fatal("Could not setup log file")
		}
		defer logFile.Close()
	}

	if cliConfig.Trace {
		logrus.SetLevel(logrus.TraceLevel)
		logrus.Trace("Trace logs enabled")
	} else if cliConfig.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debug("Debug logs enabled")
	} else {
		logrus.SetLevel(cliConfig.LogLevel)
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
