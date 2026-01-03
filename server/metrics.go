package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-kit/kit/metrics"
	"strings"
	"time"

	kitlogrus "github.com/go-kit/kit/log/logrus"
	discardMetrics "github.com/go-kit/kit/metrics/discard"
	expvarMetrics "github.com/go-kit/kit/metrics/expvar"
	kitinflux "github.com/go-kit/kit/metrics/influx"
	prometheusMetrics "github.com/go-kit/kit/metrics/prometheus"
	influx "github.com/influxdata/influxdb1-client/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
)

type MetricsBuilder interface {
	BuildConnectorMetrics() *ConnectorMetrics
	Start(ctx context.Context) error
}

const (
	MetricsBackendExpvar     = "expvar"
	MetricsBackendPrometheus = "prometheus"
	MetricsBackendInfluxDB   = "influxdb"
	MetricsBackendDiscard    = "discard"
)

type MetricsBackendConfig struct {
	Influxdb struct {
		Interval        time.Duration     `default:"1m"`
		Tags            map[string]string `usage:"any extra tags to be included with all reported metrics"`
		Addr            string
		Username        string
		Password        string
		Database        string
		RetentionPolicy string
	}
}

// NewMetricsBuilder creates a new MetricsBuilder based on the specified backend.
// If the backend is not recognized, a discard builder is returned.
// config can be nil if the backend is not influxdb.
func NewMetricsBuilder(backend string, config *MetricsBackendConfig) MetricsBuilder {
	switch strings.ToLower(backend) {
	case MetricsBackendExpvar:
		return &expvarMetricsBuilder{}
	case MetricsBackendPrometheus:
		return &prometheusMetricsBuilder{}
	case MetricsBackendInfluxDB:
		return &influxMetricsBuilder{config: config}
	case MetricsBackendDiscard:
		return &discardMetricsBuilder{}
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

type ConnectorMetrics struct {
	Errors                  metrics.Counter
	BytesTransmitted        metrics.Counter
	ConnectionsFrontend     metrics.Counter
	ConnectionsBackend      metrics.Counter
	ActiveConnections       metrics.Gauge
	ServerActivePlayer      metrics.Gauge
	ServerLogins            metrics.Counter
	ServerActiveConnections metrics.Gauge
	RateLimitAvailable      metrics.Gauge
}

func (b expvarMetricsBuilder) BuildConnectorMetrics() *ConnectorMetrics {
	c := expvarMetrics.NewCounter("connections")
	return &ConnectorMetrics{
		Errors:                  expvarMetrics.NewCounter("errors").With("subsystem", "connector"),
		BytesTransmitted:        expvarMetrics.NewCounter("bytes"),
		ConnectionsFrontend:     c,
		ConnectionsBackend:      c,
		ActiveConnections:       expvarMetrics.NewGauge("active_connections"),
		ServerActivePlayer:      expvarMetrics.NewGauge("server_active_player"),
		ServerLogins:            expvarMetrics.NewCounter("server_logins"),
		ServerActiveConnections: expvarMetrics.NewGauge("server_active_connections"),
		RateLimitAvailable:      expvarMetrics.NewGauge("rate_limit_available"),
	}
}

type discardMetricsBuilder struct {
}

func (b discardMetricsBuilder) Start(ctx context.Context) error {
	// nothing needed
	return nil
}

func (b discardMetricsBuilder) BuildConnectorMetrics() *ConnectorMetrics {
	return &ConnectorMetrics{
		Errors:                  discardMetrics.NewCounter(),
		BytesTransmitted:        discardMetrics.NewCounter(),
		ConnectionsFrontend:     discardMetrics.NewCounter(),
		ConnectionsBackend:      discardMetrics.NewCounter(),
		ActiveConnections:       discardMetrics.NewGauge(),
		ServerActivePlayer:      discardMetrics.NewGauge(),
		ServerLogins:            discardMetrics.NewCounter(),
		ServerActiveConnections: discardMetrics.NewGauge(),
		RateLimitAvailable:      discardMetrics.NewGauge(),
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

func (b *influxMetricsBuilder) BuildConnectorMetrics() *ConnectorMetrics {
	influxConfig := &b.config.Influxdb

	metrics := kitinflux.New(influxConfig.Tags, influx.BatchPointsConfig{
		Database:        influxConfig.Database,
		RetentionPolicy: influxConfig.RetentionPolicy,
	}, kitlogrus.NewLogger(logrus.StandardLogger()))

	b.metrics = metrics

	c := metrics.NewCounter("mc_router_connections")
	return &ConnectorMetrics{
		Errors:                  metrics.NewCounter("mc_router_errors"),
		BytesTransmitted:        metrics.NewCounter("mc_router_transmitted_bytes"),
		ConnectionsFrontend:     c.With("side", "frontend"),
		ConnectionsBackend:      c.With("side", "backend"),
		ActiveConnections:       metrics.NewGauge("mc_router_connections_active"),
		ServerActivePlayer:      metrics.NewGauge("mc_router_server_player_active"),
		ServerLogins:            metrics.NewCounter("mc_router_server_logins"),
		ServerActiveConnections: metrics.NewGauge("mc_router_server_active_connections"),
		RateLimitAvailable:      metrics.NewGauge("mc_router_rate_limit_available"),
	}
}

type prometheusMetricsBuilder struct {
}

var pcv *prometheusMetrics.Counter

func (b prometheusMetricsBuilder) Start(ctx context.Context) error {

	// nothing needed
	return nil
}

func (b prometheusMetricsBuilder) BuildConnectorMetrics() *ConnectorMetrics {
	pcv = prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "mc_router",
		Name:      "errors",
		Help:      "The total number of errors",
	}, []string{"type"}))
	return &ConnectorMetrics{
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
		ServerLogins: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mc_router",
			Name:      "server_logins",
			Help:      "The total number of player logins",
		}, []string{"player_name", "player_uuid", "server_address"})),
		ServerActiveConnections: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "server_active_connections",
			Help:      "The number of active connections per server",
		}, []string{"server_address"})),
		RateLimitAvailable: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "rate_limit_available",
			Help:      "The number of available tokens in the rate limit bucket",
		}, nil)),
	}
}
