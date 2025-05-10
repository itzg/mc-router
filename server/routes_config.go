package server

import (
	"context"
	"encoding/json"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"time"

	"io/fs"
	"os"
	"sync"
)

type IRoutesConfig interface {
	ReadRoutesConfig(routesConfig string)
	AddMapping(serverAddress string, backend string)
	DeleteMapping(serverAddress string)
	SetDefaultRoute(backend string)
	WatchForChanges(ctx context.Context) error
}

const debounceConfigRereadDuration = time.Second * 5

var RoutesConfig = &routesConfigImpl{}

type routesConfigImpl struct {
	sync.RWMutex
	fileName string
}

type routesConfigStructure struct {
	DefaultServer string            `json:"default-server"`
	Mappings      map[string]string `json:"mappings"`
}

func (r *routesConfigImpl) ReadRoutesConfig(routesConfig string) error {
	r.fileName = routesConfig

	logrus.WithField("routesConfig", r.fileName).Info("Loading routes config file")

	config, readErr := r.readRoutesConfigFile()

	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			logrus.WithField("routesConfig", r.fileName).Info("Routes config file doses not exist, skipping reading it")
			// File doesn't exist -> ignore it
			return nil
		}
		return errors.Wrap(readErr, "Could not load the routes config file")
	}

	Routes.RegisterAll(config.Mappings)
	Routes.SetDefaultRoute(config.DefaultServer)
	return nil
}

func (r *routesConfigImpl) reloadRoutesConfig() error {
	config, readErr := r.readRoutesConfigFile()

	if readErr != nil {
		return readErr
	}

	logrus.WithField("routesConfig", r.fileName).Info("Re-loading routes config file")
	Routes.Reset()
	Routes.RegisterAll(config.Mappings)
	Routes.SetDefaultRoute(config.DefaultServer)

	return nil
}

func (r *routesConfigImpl) WatchForChanges(ctx context.Context) error {
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
				readErr := r.ReadRoutesConfig(r.fileName)
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

func (r *routesConfigImpl) AddMapping(serverAddress string, backend string) {
	if !r.isRoutesConfigEnabled() {
		return
	}

	config, readErr := r.readRoutesConfigFile()
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		logrus.WithError(readErr).Error("Could not read the routes config file")
		return
	}
	if config.Mappings == nil {
		config.Mappings = make(map[string]string)
	}

	config.Mappings[serverAddress] = backend

	writeErr := r.writeRoutesConfigFile(config)
	if writeErr != nil {
		logrus.WithError(writeErr).Error("Could not write to the routes config file")
		return
	}

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Added route to routes config")

	return
}

func (r *routesConfigImpl) SetDefaultRoute(backend string) {
	if !r.isRoutesConfigEnabled() {
		return
	}

	config, readErr := r.readRoutesConfigFile()
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		logrus.WithError(readErr).Error("Could not read the routes config file")
		return
	}

	config.DefaultServer = backend

	writeErr := r.writeRoutesConfigFile(config)
	if writeErr != nil {
		logrus.WithError(writeErr).Error("Could not write to the routes config file")
		return
	}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Set default route in routes config")

	return
}

func (r *routesConfigImpl) DeleteMapping(serverAddress string) {
	if !r.isRoutesConfigEnabled() {
		return
	}

	config, readErr := r.readRoutesConfigFile()
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		logrus.WithError(readErr).Error("Could not read the routes config file")
		return
	}

	delete(config.Mappings, serverAddress)

	writeErr := r.writeRoutesConfigFile(config)
	if writeErr != nil {
		logrus.WithError(writeErr).Error("Could not write to the routes config file")
		return
	}

	logrus.WithField("serverAddress", serverAddress).Info("Deleted route in routes config")

	return
}

func (r *routesConfigImpl) isRoutesConfigEnabled() bool {
	return r.fileName != ""
}

func (r *routesConfigImpl) readRoutesConfigFile() (routesConfigStructure, error) {
	r.RLock()
	defer r.RUnlock()

	config := routesConfigStructure{
		"",
		make(map[string]string),
	}

	file, fileErr := os.ReadFile(r.fileName)
	if fileErr != nil {
		return config, errors.Wrap(fileErr, "Could not load the routes config file")
	}

	parseErr := json.Unmarshal(file, &config)
	if parseErr != nil {
		return config, errors.Wrap(parseErr, "Could not parse the json routes config file")
	}

	return config, nil
}

func (r *routesConfigImpl) writeRoutesConfigFile(config routesConfigStructure) error {
	r.Lock()
	defer r.Unlock()

	newFileContent, parseErr := json.Marshal(config)
	if parseErr != nil {
		return errors.Wrap(parseErr, "Could not parse the routes to json")
	}

	fileErr := os.WriteFile(r.fileName, newFileContent, 0664)
	if fileErr != nil {
		return errors.Wrap(fileErr, "Could not write to the routes config file")
	}

	return nil
}
