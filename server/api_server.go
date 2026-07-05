package server

import (
	"context"
	"encoding/json"
	"expvar"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// apiServer holds the dependencies the REST handlers need, injected at startup
// rather than reached for as globals.
type apiServer struct {
	routes       IRoutes
	configLoader *RoutesConfigLoader
	scaler       *WebhookScaler
}

func StartApiServer(apiBinding string, routes IRoutes, configLoader *RoutesConfigLoader, scaler *WebhookScaler) {
	logrus.WithField("binding", apiBinding).Info("Serving API requests")

	api := &apiServer{routes: routes, configLoader: configLoader, scaler: scaler}

	var apiRoutes = mux.NewRouter()
	api.registerApiRoutes(apiRoutes)

	apiRoutes.Path("/vars").Handler(expvar.Handler())

	apiRoutes.Path("/metrics").Handler(promhttp.Handler())

	go func() {
		logrus.WithError(
			http.ListenAndServe(apiBinding, apiRoutes)).Error("API server failed")
	}()
}

func (a *apiServer) registerApiRoutes(apiRoutes *mux.Router) {
	apiRoutes.Path("/routes").Methods("GET").
		HandlerFunc(a.routesListHandler)
	apiRoutes.Path("/routes").Methods("POST").
		HandlerFunc(a.routesCreateHandler)
	apiRoutes.Path("/defaultRoute").Methods("POST").
		HandlerFunc(a.routesSetDefault)
	apiRoutes.Path("/routes/{serverAddress}").Methods("DELETE").HandlerFunc(a.routesDeleteHandler)
}

func (a *apiServer) routesListHandler(writer http.ResponseWriter, _ *http.Request) {
	type serverRoute = struct {
		Backend       string `json:"backend"`
		ScalingTarget string `json:"scalingTarget"`
	}

	mappings := a.routes.GetMappings()
	routes := make(map[string]serverRoute, len(mappings))
	for k := range mappings {
		backend, address, scalingTarget, _, _ := a.routes.FindBackendForServerAddress(context.Background(), k)
		routes[address] = serverRoute{Backend: backend, ScalingTarget: scalingTarget}
	}

	bytes, err := json.Marshal(routes)
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

func (a *apiServer) routesDeleteHandler(writer http.ResponseWriter, request *http.Request) {
	serverAddress := mux.Vars(request)["serverAddress"]
	if serverAddress != "" {
		if a.routes.DeleteMapping(serverAddress) {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusNotFound)
		}
		a.configLoader.SaveRoutes()
	}
}

func (a *apiServer) routesCreateHandler(writer http.ResponseWriter, request *http.Request) {
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

	waker, sleeper := a.scaler.routeFuncs(definition.ServerAddress, definition.Backend)
	a.routes.CreateMapping(definition.ServerAddress, definition.Backend, "", waker, sleeper, "", "", 0)
	a.configLoader.SaveRoutes()
	writer.WriteHeader(http.StatusCreated)
}

func (a *apiServer) routesSetDefault(writer http.ResponseWriter, request *http.Request) {
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

	waker, sleeper := a.scaler.routeFuncs("", body.Backend)
	a.routes.SetDefaultRoute(body.Backend, "", waker, sleeper, "", "", 0)
	a.configLoader.SaveRoutes()
	writer.WriteHeader(http.StatusOK)
}
