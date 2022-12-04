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
	fileName string
}

type routesConfigStructure struct {
	DefaultServer string `json:"default-server"`
	Mappings map[string]string `json:"mappings"`
}


func (r *routesConfigImpl) ReadRoutesConfig(routesConfig string) error {
	r.fileName = routesConfig

	config, readErr := r.readRoutesConfigFile()

	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			logrus.WithField("routesConfig", r.fileName).Info("Config file doses not exist, skipping reading it")
			// File doesn't exist -> ignore it
			return nil
		}
		return errors.Wrap(readErr, "Could not load the routes config file")
	}

	Routes.RegisterAll(config.Mappings)
	Routes.SetDefaultRoute(config.DefaultServer)
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
		logrus.WithError(writeErr).Error("Could not write to the config file")
		return
	}

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend": backend,
	}).Info("Added route to config")

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
		logrus.WithError(writeErr).Error("Could not write to the config file")
		return
	}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Set default route in config")

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
		logrus.WithError(writeErr).Error("Could not write to the config file")
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
		return errors.Wrap(parseErr, "Could not parse the route mappings to json")
	}

	fileErr := os.WriteFile(r.fileName, newFileContent, 0664)
	if fileErr != nil {
		return errors.Wrap(fileErr, "Could not write the routes config file")
	}

	return nil
}
