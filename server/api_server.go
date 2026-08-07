package server

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"net"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// ApiServer holds the dependencies the REST handlers need, injected at startup
// rather than reached for as globals.
type ApiServer struct {
	ctx           context.Context
	routes        IRoutes
	configLoader  *RoutesConfigLoader
	webhookScaler *WebhookScaler
	addr          string
}

// StartApiServer starts the REST API server. It will listen on apiBinding's host:port utilizing the
// given routes to retrieve routes, configLoader to save routes, and scaler to scale routes via a WebhookScaler.
func StartApiServer(ctx context.Context, apiBinding string, routes IRoutes, configLoader *RoutesConfigLoader, webhookScaler *WebhookScaler) (*ApiServer, error) {
	api := &ApiServer{ctx: ctx, routes: routes, configLoader: configLoader, webhookScaler: webhookScaler}

	var apiRoutes = mux.NewRouter()
	api.registerApiRoutes(apiRoutes)

	apiRoutes.Path("/vars").Handler(expvar.Handler())

	apiRoutes.Path("/metrics").Handler(promhttp.Handler())

	ln, err := net.Listen("tcp", apiBinding)
	if err != nil {
		logrus.WithError(err).Error("API server failed to start")
		return nil, fmt.Errorf("failed to start API server: %w", err)
	}
	api.addr = ln.Addr().String()

	server := &http.Server{Addr: apiBinding, Handler: apiRoutes}
	server.BaseContext = func(_ net.Listener) context.Context { return ctx }

	context.AfterFunc(ctx, func() {
		_ = server.Shutdown(ctx)
	})

	go func() {
		logrus.WithField("binding", api.addr).Info("Serving API requests")

		logrus.WithError(
			server.Serve(ln),
		).Error("API server failed")
	}()

	return api, nil
}

func (a *ApiServer) GetAddr() string {
	return a.addr
}

func (a *ApiServer) registerApiRoutes(apiRoutes *mux.Router) {
	apiRoutes.Path("/routes").Methods("GET").
		HandlerFunc(a.routesListHandler)
	apiRoutes.Path("/routes").Methods("POST").
		HandlerFunc(a.routesCreateHandler)
	apiRoutes.Path("/defaultRoute").Methods("POST").
		HandlerFunc(a.routesSetDefault)
	apiRoutes.Path("/routes/{serverAddress}").Methods("DELETE").HandlerFunc(a.routesDeleteHandler)
}

func (a *ApiServer) routesListHandler(writer http.ResponseWriter, _ *http.Request) {
	type serverRoute = struct {
		Backend       string `json:"backend"`
		ScalingTarget string `json:"scalingTarget"`
	}

	mappings := a.routes.GetMappings()
	routes := make(map[string]serverRoute, len(mappings))
	for k := range mappings {
		backend, address, scalingTarget, _, _ := a.routes.FindBackendForServerAddress(a.ctx, k)
		routes[address] = serverRoute{Backend: backend, ScalingTarget: safeScalingKey(scalingTarget)}
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

func (a *ApiServer) routesDeleteHandler(writer http.ResponseWriter, request *http.Request) {
	serverAddress := mux.Vars(request)["serverAddress"]
	if serverAddress != "" {
		if a.routes.RemoveMapping(serverAddress) {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusNotFound)
		}
		if a.configLoader != nil {
			a.configLoader.SaveRoutes()
		}
	}
}

func (a *ApiServer) routesCreateHandler(writer http.ResponseWriter, request *http.Request) {
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

	waker, sleeper, scalingTarget := a.webhookScaler.routeFuncs(definition.ServerAddress, definition.Backend)
	a.routes.CreateMapping(definition.ServerAddress, definition.Backend, scalingTarget, waker, sleeper, "", "")
	if a.configLoader != nil {
		a.configLoader.SaveRoutes()
	}
	writer.WriteHeader(http.StatusCreated)
}

func (a *ApiServer) routesSetDefault(writer http.ResponseWriter, request *http.Request) {
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

	waker, sleeper, scalingTarget := a.webhookScaler.routeFuncs("", body.Backend)
	a.routes.SetDefaultRoute(body.Backend, scalingTarget, waker, sleeper, "", "")
	if a.configLoader != nil {
		a.configLoader.SaveRoutes()
	}
	writer.WriteHeader(http.StatusOK)
}
