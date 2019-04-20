package server

import (
	"encoding/json"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"net/http"
	"sync"
)

func init() {
	apiRoutes.Path("/routes").Methods("GET").
		Headers("Accept", "application/json").
		HandlerFunc(routesListHandler)
	apiRoutes.Path("/routes").Methods("POST").
		Headers("Content-Type", "application/json").
		HandlerFunc(routesCreateHandler)
	apiRoutes.Path("/defaultRoute").Methods("POST").
		Headers("Content-Type", "application/json").
		HandlerFunc(routesSetDefault)
	apiRoutes.Path("/routes/{serverAddress}").Methods("DELETE").HandlerFunc(routesDeleteHandler)
}

func routesListHandler(writer http.ResponseWriter, request *http.Request) {
	mappings := Routes.GetMappings()
	bytes, err := json.Marshal(mappings)
	if err != nil {
		logrus.WithError(err).Error("Failed to marshal mappings")
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Write(bytes)
}

func routesDeleteHandler(writer http.ResponseWriter, request *http.Request) {
	serverAddress := mux.Vars(request)["serverAddress"]
	if serverAddress != "" {
		if Routes.DeleteMapping(serverAddress) {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusNotFound)
		}
	}
}

func routesCreateHandler(writer http.ResponseWriter, request *http.Request) {
	var definition = struct {
		ServerAddress string
		Backend       string
	}{}

	defer request.Body.Close()

	decoder := json.NewDecoder(request.Body)
	err := decoder.Decode(&definition)
	if err != nil {
		logrus.WithError(err).Error("Unable to get request body")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	Routes.CreateMapping(definition.ServerAddress, definition.Backend)
	writer.WriteHeader(http.StatusCreated)
}

func routesSetDefault(writer http.ResponseWriter, request *http.Request) {
	var body = struct {
		Backend string
	}{}

	defer request.Body.Close()

	decoder := json.NewDecoder(request.Body)
	err := decoder.Decode(&body)
	if err != nil {
		logrus.WithError(err).Error("Unable to parse request")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	Routes.SetDefaultRoute(body.Backend)
	writer.WriteHeader(http.StatusOK)
}

type IRoutes interface {
	RegisterAll(mappings map[string]string)
	// FindBackendForServerAddress returns the host:port for the external server address, if registered.
	// Otherwise, an empty string is returned
	FindBackendForServerAddress(serverAddress string) string
	GetMappings() map[string]string
	DeleteMapping(serverAddress string) bool
	CreateMapping(serverAddress string, backend string)
	SetDefaultRoute(backend string)
}

var Routes IRoutes = &routesImpl{}

func (r *routesImpl) RegisterAll(mappings map[string]string) {
	r.Lock()
	defer r.Unlock()

	r.mappings = mappings
}

type routesImpl struct {
	sync.RWMutex
	mappings     map[string]string
	defaultRoute string
}

func (r *routesImpl) SetDefaultRoute(backend string) {
	r.defaultRoute = backend

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")
}

func (r *routesImpl) FindBackendForServerAddress(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if r.mappings == nil {
		return r.defaultRoute
	} else {

		if route, exists := r.mappings[serverAddress]; exists {
			return route
		} else {
			return r.defaultRoute
		}
	}
}

func (r *routesImpl) GetMappings() map[string]string {
	r.RLock()
	defer r.RUnlock()

	result := make(map[string]string, len(r.mappings))
	for k, v := range r.mappings {
		result[k] = v
	}
	return result
}

func (r *routesImpl) DeleteMapping(serverAddress string) bool {
	r.Lock()
	defer r.Unlock()
	logrus.WithField("serverAddress", serverAddress).Info("Deleting route")

	if _, ok := r.mappings[serverAddress]; ok {
		delete(r.mappings, serverAddress)
		return true
	} else {
		return false
	}
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string) {
	r.Lock()
	defer r.Unlock()

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Creating route")
	r.mappings[serverAddress] = backend
}
