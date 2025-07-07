package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"io/fs"
	"os"
)

const debounceConfigRereadDuration = time.Second * 5

var RoutesConfigLoader = &routesConfigLoader{}

type routesConfigLoader struct {
	fileName string
}

// RoutesConfigSchema declares the schema of the json file that can provide routes to serve
type RoutesConfigSchema struct {
	DefaultServer string            `json:"default-server"`
	Mappings      map[string]string `json:"mappings"`
}

func (r *routesConfigLoader) Load(routesConfigFileName string) error {
	r.fileName = routesConfigFileName

	logrus.WithField("routesConfigFileName", r.fileName).Info("Loading routes config file")

	config, readErr := r.readFile()

	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			logrus.WithField("routesConfigFileName", r.fileName).Info("Routes config file doses not exist, skipping reading it")
			// File doesn't exist -> ignore it
			return nil
		}
		return errors.Wrap(readErr, "Could not load the routes config file")
	}

	Routes.RegisterAll(config.Mappings)
	Routes.SetDefaultRoute(config.DefaultServer)
	return nil
}

func (r *routesConfigLoader) Reload() error {
	config, readErr := r.readFile()

	if readErr != nil {
		return readErr
	}

	logrus.WithField("routesConfig", r.fileName).Info("Re-loading routes config file")
	Routes.Reset()
	Routes.RegisterAll(config.Mappings)
	Routes.SetDefaultRoute(config.DefaultServer)

	return nil
}

func (r *routesConfigLoader) WatchForChanges(ctx context.Context) error {
	if r.fileName == "" {
		return errors.New("routes config file needs to be specified first")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.Wrap(err, "Could not create a watcher")
	}

	err = watcher.Add(r.fileName)
	if err != nil {
		return errors.Wrap(err, "Could not watch the routes config file")
	}

	go func() {
		logrus.WithField("file", r.fileName).Info("Watching routes config file")

		debounceTimerChan := make(<-chan time.Time)
		var debounceTimer *time.Timer

		//goland:noinspection GoUnhandledErrorResult
		defer watcher.Close()
		for {
			select {

			case event, ok := <-watcher.Events:
				if !ok {
					logrus.Debug("Watcher events channel closed")
					return
				}
				logrus.
					WithField("file", event.Name).
					WithField("op", event.Op).
					Trace("fs event received")
				if event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Create) {
					if debounceTimer == nil {
						debounceTimer = time.NewTimer(debounceConfigRereadDuration)
					} else {
						debounceTimer.Reset(debounceConfigRereadDuration)
					}
					debounceTimerChan = debounceTimer.C
					logrus.WithField("delay", debounceConfigRereadDuration).Debug("Will re-read config file after delay")
				}

			case <-debounceTimerChan:
				readErr := r.Load(r.fileName)
				if readErr != nil {
					logrus.
						WithError(readErr).
						WithField("routesConfig", r.fileName).
						Error("Could not re-read the routes config file")
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

func (r *routesConfigLoader) SaveRoutes() {
	if !r.isEnabled() {
		return
	}

	err := r.writeFile(&RoutesConfigSchema{
		DefaultServer: Routes.GetDefaultRoute(),
		Mappings:      Routes.GetMappings(),
	})
	if err != nil {
		logrus.WithError(err).Error("Could not save the routes config file")
		return
	}
	logrus.Info("Saved routes config")
}

func (r *routesConfigLoader) isEnabled() bool {
	return r.fileName != ""
}

func (r *routesConfigLoader) readFile() (*RoutesConfigSchema, error) {
	var config RoutesConfigSchema

	content, err := os.ReadFile(r.fileName)
	if err != nil {
		return &config, errors.Wrap(err, "Could not load the routes config file")
	}

	parseErr := json.Unmarshal(content, &config)
	if parseErr != nil {
		return &config, errors.Wrap(parseErr, "Could not parse the json routes config file")
	}

	return &config, nil
}

func (r *routesConfigLoader) writeFile(config *RoutesConfigSchema) error {
	newFileContent, err := json.Marshal(config)
	if err != nil {
		return errors.Wrap(err, "Could not parse the routes to json")
	}

	err = os.WriteFile(r.fileName, newFileContent, 0664)
	if err != nil {
		return errors.Wrap(err, "Could not write to the routes config file")
	}

	return nil
}
