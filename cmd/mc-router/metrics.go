package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kitlogrus "github.com/go-kit/kit/log/logrus"
	discardMetrics "github.com/go-kit/kit/metrics/discard"
	expvarMetrics "github.com/go-kit/kit/metrics/expvar"
	kitinflux "github.com/go-kit/kit/metrics/influx"
	prometheusMetrics "github.com/go-kit/kit/metrics/prometheus"
	influx "github.com/influxdata/influxdb1-client/v2"
	"github.com/itzg/mc-router/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
)

type MetricsBuilder interface {
	BuildConnectorMetrics() *server.ConnectorMetrics
	Start(ctx context.Context) error
}

func NewMetricsBuilder(backend string, config *MetricsBackendConfig) MetricsBuilder {
	switch strings.ToLower(backend) {
	case "expvar":
		return &expvarMetricsBuilder{}
	case "prometheus":
		return &prometheusMetricsBuilder{}
	case "influxdb":
		return &influxMetricsBuilder{config: config}
	default:
		return &discardMetricsBuilder{}
	}
}

type expvarMetricsBuilder struct {
}

func (b expvarMetricsBuilder) Start(ctx context.Context) error {
	// nothing needed
	return nil
}

func (b expvarMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	c := expvarMetrics.NewCounter("connections")
	return &server.ConnectorMetrics{
		Errors:              expvarMetrics.NewCounter("errors").With("subsystem", "connector"),
		BytesTransmitted:    expvarMetrics.NewCounter("bytes"),
		ConnectionsFrontend: c,
		ConnectionsBackend:  c,
		ActiveConnections:   expvarMetrics.NewGauge("active_connections"),
		ServerActivePlayer:  expvarMetrics.NewGauge("server_active_player"),
	}
}

type discardMetricsBuilder struct {
}

func (b discardMetricsBuilder) Start(ctx context.Context) error {
	// nothing needed
	return nil
}

func (b discardMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	return &server.ConnectorMetrics{
		Errors:              discardMetrics.NewCounter(),
		BytesTransmitted:    discardMetrics.NewCounter(),
		ConnectionsFrontend: discardMetrics.NewCounter(),
		ConnectionsBackend:  discardMetrics.NewCounter(),
		ActiveConnections:   discardMetrics.NewGauge(),
		ServerActivePlayer:  discardMetrics.NewGauge(),
	}
}

type influxMetricsBuilder struct {
	config  *MetricsBackendConfig
	metrics *kitinflux.Influx
}

func (b *influxMetricsBuilder) Start(ctx context.Context) error {
	influxConfig := &b.config.Influxdb
	if influxConfig.Addr == "" {
		return errors.New("influx addr is required")
	}

	ticker := time.NewTicker(influxConfig.Interval)
	client, err := influx.NewHTTPClient(influx.HTTPConfig{
		Addr:     influxConfig.Addr,
		Username: influxConfig.Username,
		Password: influxConfig.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create influx http client: %w", err)
	}

	go b.metrics.WriteLoop(ctx, ticker.C, client)

	logrus.WithField("addr", influxConfig.Addr).
		Debug("reporting metrics to influxdb")

	return nil
}

func (b *influxMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	influxConfig := &b.config.Influxdb

	metrics := kitinflux.New(influxConfig.Tags, influx.BatchPointsConfig{
		Database:        influxConfig.Database,
		RetentionPolicy: influxConfig.RetentionPolicy,
	}, kitlogrus.NewLogger(logrus.StandardLogger()))

	b.metrics = metrics

	c := metrics.NewCounter("mc_router_connections")
	return &server.ConnectorMetrics{
		Errors:              metrics.NewCounter("mc_router_errors"),
		BytesTransmitted:    metrics.NewCounter("mc_router_transmitted_bytes"),
		ConnectionsFrontend: c.With("side", "frontend"),
		ConnectionsBackend:  c.With("side", "backend"),
		ActiveConnections:   metrics.NewGauge("mc_router_connections_active"),
		ServerActivePlayer:  metrics.NewGauge("mc_router_server_player_active"),
	}
}

type prometheusMetricsBuilder struct {
}

var pcv *prometheusMetrics.Counter

func (b prometheusMetricsBuilder) Start(ctx context.Context) error {

	// nothing needed
	return nil
}

func (b prometheusMetricsBuilder) BuildConnectorMetrics() *server.ConnectorMetrics {
	pcv = prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "mc_router",
		Name:      "errors",
		Help:      "The total number of errors",
	}, []string{"type"}))
	return &server.ConnectorMetrics{
		Errors: pcv,
		BytesTransmitted: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mc_router",
			Name:      "bytes",
			Help:      "The total number of bytes transmitted",
		}, nil)),
		ConnectionsFrontend: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "mc_router",
			Subsystem:   "frontend",
			Name:        "connections",
			Help:        "The total number of connections",
			ConstLabels: prometheus.Labels{"side": "frontend"},
		}, nil)),
		ConnectionsBackend: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "mc_router",
			Subsystem:   "backend",
			Name:        "connections",
			Help:        "The total number of backend connections",
			ConstLabels: prometheus.Labels{"side": "backend"},
		}, []string{"host"})),
		ActiveConnections: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "active_connections",
			Help:      "The number of active connections",
		}, nil)),
		ServerActivePlayer: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "server_active_player",
			Help:      "Player is active on server",
		}, []string{"player_name", "player_uuid", "server_address"})),
	}
}
