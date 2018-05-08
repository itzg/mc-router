package server

import (
	"net/http"
	"github.com/sirupsen/logrus"
	"github.com/gorilla/mux"
)

var apiRoutes = mux.NewRouter()

func StartApiServer(apiBinding string) {
	logrus.WithField("binding", apiBinding).Info("Serving API requests")
	go func() {
		logrus.WithError(
			http.ListenAndServe(apiBinding, apiRoutes)).Error("API server failed")
	}()
}
