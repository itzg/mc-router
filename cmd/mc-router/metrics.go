package main

import (
	discardMetrics "github.com/go-kit/kit/metrics/discard"
	expvarMetrics "github.com/go-kit/kit/metrics/expvar"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
)

type MetricsBuilder interface {
	BuildConnectorMetrics() *server.ConnectorMetrics
}

func NewMetricsBuilder() MetricsBuilder {
	switch *metricsBackend {
	case "discard":
		return &discardMetricsBuilder{}
	case "expvar":
		return &expvarMetricsBuilder{}
	default:
		logrus.Fatalf("Unsupported metrics backend: %s", *metricsBackend)
		return nil
	}
}

type expvarMetricsBuilder struct {
}

func (b expvarMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	return &server.ConnectorMetrics{
		Errors:            expvarMetrics.NewCounter("errors").With("subsystem", "connector"),
		BytesTransmitted:  expvarMetrics.NewCounter("bytes"),
		Connections:       expvarMetrics.NewCounter("connections"),
		ActiveConnections: expvarMetrics.NewGauge("active_connections"),
	}
}

type discardMetricsBuilder struct {
}

func (b discardMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	return &server.ConnectorMetrics{
		Errors:            discardMetrics.NewCounter(),
		BytesTransmitted:  discardMetrics.NewCounter(),
		Connections:       discardMetrics.NewCounter(),
		ActiveConnections: discardMetrics.NewGauge(),
	}
}
