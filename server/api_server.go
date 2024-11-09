package server

import (
	"expvar"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var apiRoutes = mux.NewRouter()

func StartApiServer(apiBinding string) {
	logrus.WithField("binding", apiBinding).Info("Serving API requests")

	apiRoutes.Path("/vars").Handler(expvar.Handler())

	apiRoutes.Path("/metrics").Handler(promhttp.Handler())

	go func() {
		logrus.WithError(
			http.ListenAndServe(apiBinding, apiRoutes)).Error("API server failed")
	}()
}
