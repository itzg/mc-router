package server

import (
	"encoding/json"
	"expvar"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

func StartApiServer(apiBinding string) {
	logrus.WithField("binding", apiBinding).Info("Serving API requests")

	var apiRoutes = mux.NewRouter()
	registerApiRoutes(apiRoutes)

	apiRoutes.Path("/vars").Handler(expvar.Handler())

	apiRoutes.Path("/metrics").Handler(promhttp.Handler())

	go func() {
		logrus.WithError(
			http.ListenAndServe(apiBinding, apiRoutes)).Error("API server failed")
	}()
}

func registerApiRoutes(apiRoutes *mux.Router) {
	apiRoutes.Path("/routes").Methods("GET").
		HandlerFunc(routesListHandler)
	apiRoutes.Path("/routes").Methods("POST").
		HandlerFunc(routesCreateHandler)
	apiRoutes.Path("/defaultRoute").Methods("POST").
		HandlerFunc(routesSetDefault)
	apiRoutes.Path("/routes/{serverAddress}").Methods("DELETE").HandlerFunc(routesDeleteHandler)
}

func routesListHandler(writer http.ResponseWriter, _ *http.Request) {
	mappings := Routes.GetMappings()
	bytes, err := json.Marshal(mappings)
	if err != nil {
		logrus.WithError(err).Error("Failed to marshal mappings")
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(bytes)
	if err != nil {
		logrus.WithError(err).Error("Failed to write response")
	}
}

func routesDeleteHandler(writer http.ResponseWriter, request *http.Request) {
	serverAddress := mux.Vars(request)["serverAddress"]
	if serverAddress != "" {
		if Routes.DeleteMapping(serverAddress) {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusNotFound)
		}
		RoutesConfigLoader.SaveRoutes()
	}
}

func routesCreateHandler(writer http.ResponseWriter, request *http.Request) {
	var definition = struct {
		ServerAddress string
		Backend       string
	}{}

	//goland:noinspection GoUnhandledErrorResult
	defer request.Body.Close()

	decoder := json.NewDecoder(request.Body)
	err := decoder.Decode(&definition)
	if err != nil {
		logrus.WithError(err).Error("Unable to get request body")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	Routes.CreateMapping(definition.ServerAddress, definition.Backend, nil, nil)
	RoutesConfigLoader.SaveRoutes()
	writer.WriteHeader(http.StatusCreated)
}

func routesSetDefault(writer http.ResponseWriter, request *http.Request) {
	var body = struct {
		Backend string
	}{}

	//goland:noinspection GoUnhandledErrorResult
	defer request.Body.Close()

	decoder := json.NewDecoder(request.Body)
	err := decoder.Decode(&body)
	if err != nil {
		logrus.WithError(err).Error("Unable to parse request")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	Routes.SetDefaultRoute(body.Backend, nil, nil)
	RoutesConfigLoader.SaveRoutes()
	writer.WriteHeader(http.StatusOK)
}
