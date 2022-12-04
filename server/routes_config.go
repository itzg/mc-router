package server

import (
	"encoding/json"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"io/fs"
	"os"
	"sync"
)

type IRoutesConfig interface {
	ReadRoutesConfig(routesConfig string)
	AddMapping(serverAddress string, backend string)
	DeleteMapping(serverAddress string)
	SetDefaultRoute(backend string)
}

var RoutesConfig = &routesConfigImpl{}

type routesConfigImpl struct {
	sync.RWMutex
	name string
}

func (r *routesConfigImpl) ReadRoutesConfig(routesConfig string) error {
	r.name = routesConfig

	configMappings, readErr := r.readRoutesConfigFile()

	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			logrus.WithField("routesConfig", r.name).Info("Config file doses not exist, skipping reading it")
			// File doesn't exist -> ignore it
			return nil
		}
		return errors.Wrap(readErr, "Could not load the routes config file")
	}

	Routes.RegisterAll(configMappings)
	return nil
}

func (r *routesConfigImpl) AddMapping(serverAddress string, backend string) {
	if !r.isRoutesConfigEnabled() {
		return
	}

	configMappings, readErr := r.readRoutesConfigFile()
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		logrus.WithError(readErr).Error("Could not read the routes config file")
		return
	}
	if configMappings == nil {
		configMappings = make(map[string]string)
	}

	configMappings[serverAddress] = backend

	writeErr := r.writeRoutesConfigFile(configMappings)
	if writeErr != nil {
		logrus.WithError(writeErr).Error("Could not write to the config file")
		return
	}

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend": backend,
	}).Info("Added route to config")

	return
}

func (r *routesConfigImpl) DeleteMapping(serverAddress string) {
	if !r.isRoutesConfigEnabled() {
		return
	}

	configMappings, readErr := r.readRoutesConfigFile()
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		logrus.WithError(readErr).Error("Could not read the routes config file")
		return
	}

	delete(configMappings, serverAddress)

	writeErr := r.writeRoutesConfigFile(configMappings)
	if writeErr != nil {
		logrus.WithError(writeErr).Error("Could not write to the config file")
		return
	}

	logrus.WithField("serverAddress", serverAddress).Info("Deleted route in routes config")

	return
}


func (r *routesConfigImpl) isRoutesConfigEnabled() bool {
	return r.name != ""
}

func (r *routesConfigImpl) readRoutesConfigFile() (map[string]string, error) {
	r.RLock()
	defer r.RUnlock()

	file, fileErr := os.ReadFile(r.name)
	if fileErr != nil {
		return nil, errors.Wrap(fileErr, "Could not load the routes config file")
	}

	configMappings := make(map[string]string)

	parseErr := json.Unmarshal(file, &configMappings)
	if parseErr != nil {
		return nil, errors.Wrap(parseErr, "Could not parse the json routes config file")
	}

	return configMappings, nil
}

func (r *routesConfigImpl) writeRoutesConfigFile(configMappings map[string]string) error {
	r.Lock()
	defer r.Unlock()

	newFileContent, parseErr := json.Marshal(configMappings)
	if parseErr != nil {
		return errors.Wrap(parseErr, "Could not parse the route mappings to json")
	}

	fileErr := os.WriteFile(r.name, newFileContent, 0664)
	if fileErr != nil {
		return errors.Wrap(fileErr, "Could not write the routes config file")
	}

	return nil
}
