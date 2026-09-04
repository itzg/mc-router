package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-kit/kit/metrics"

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
	BuildConnectorMetrics() ConnectorMetrics
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

type ConnectorMetrics interface {
	IncrementErrors(errorType string)
	AddBytesTransmitted(amount int64)
	IncrementConnectionsFrontend()
	IncrementConnectionsBackend(host string)
	SetActiveConnections(count int32)
	SetServerActivePlayerCounts(playerName string, playerUuid string, serverAddress string, count int)
	IncrementServerLogins(playerName string, playerUuid string, serverAddress string)
	SetServerActiveConnections(serverAddress string, value int)
	SetRateLimitAvailable(value int64)
}

type goKitConnectorMetrics struct {
	errorsCounter           metrics.Counter
	bytesTransmitted        metrics.Counter
	connectionsFrontend     metrics.Counter
	connectionsBackend      metrics.Counter
	activeConnections       metrics.Gauge
	serverActivePlayer      metrics.Gauge
	serverLogins            metrics.Counter
	serverActiveConnections metrics.Gauge
	rateLimitAvailable      metrics.Gauge
}

func (cm *goKitConnectorMetrics) IncrementErrors(errorType string) {
	cm.errorsCounter.With("type", errorType).Add(1)
}

func (cm *goKitConnectorMetrics) AddBytesTransmitted(amount int64) {
	cm.bytesTransmitted.Add(float64(amount))
}

func (cm *goKitConnectorMetrics) IncrementConnectionsFrontend() {
	cm.connectionsFrontend.Add(1)
}

// IncrementConnectionsBackend increments the counter for backend connections for the given host, which must be sanitized
func (cm *goKitConnectorMetrics) IncrementConnectionsBackend(host string) {
	cm.connectionsBackend.With("host", host).Add(1)
}

func (cm *goKitConnectorMetrics) SetActiveConnections(count int32) {
	cm.activeConnections.Set(float64(count))
}

// SetServerActivePlayerCounts sets the count of active players for the given player and server, all of which must be sanitized
func (cm *goKitConnectorMetrics) SetServerActivePlayerCounts(playerName string, playerUuid string, serverAddress string, count int) {
	cm.serverActivePlayer.
		With("player_name", playerName).
		With("player_uuid", playerUuid).
		With("server_address", serverAddress).
		Set(float64(count))
}

// IncrementServerLogins increments the count of server logins for the given player and server, all of which must be sanitized
func (cm *goKitConnectorMetrics) IncrementServerLogins(playerName string, playerUuid string, serverAddress string) {
	cm.serverLogins.With("player_name", playerName).With("player_uuid", playerUuid).With("server_address", serverAddress).Add(1)
}

// SetServerActiveConnections sets the count of active connections for the given server, which must be sanitized
func (cm *goKitConnectorMetrics) SetServerActiveConnections(serverAddress string, value int) {
	cm.serverActiveConnections.With("server_address", serverAddress).Set(float64(value))
}

func (cm *goKitConnectorMetrics) SetRateLimitAvailable(value int64) {
	cm.rateLimitAvailable.Set(float64(value))
}

func (b expvarMetricsBuilder) BuildConnectorMetrics() ConnectorMetrics {
	c := expvarMetrics.NewCounter("connections")
	return &goKitConnectorMetrics{
		errorsCounter:           expvarMetrics.NewCounter("errors").With("subsystem", "connector"),
		bytesTransmitted:        expvarMetrics.NewCounter("bytes"),
		connectionsFrontend:     c,
		connectionsBackend:      c,
		activeConnections:       expvarMetrics.NewGauge("active_connections"),
		serverActivePlayer:      expvarMetrics.NewGauge("server_active_player"),
		serverLogins:            expvarMetrics.NewCounter("server_logins"),
		serverActiveConnections: expvarMetrics.NewGauge("server_active_connections"),
		rateLimitAvailable:      expvarMetrics.NewGauge("rate_limit_available"),
	}
}

type discardMetricsBuilder struct {
}

func (b discardMetricsBuilder) Start(ctx context.Context) error {
	// nothing needed
	return nil
}

func (b discardMetricsBuilder) BuildConnectorMetrics() ConnectorMetrics {
	return &goKitConnectorMetrics{
		errorsCounter:           discardMetrics.NewCounter(),
		bytesTransmitted:        discardMetrics.NewCounter(),
		connectionsFrontend:     discardMetrics.NewCounter(),
		connectionsBackend:      discardMetrics.NewCounter(),
		activeConnections:       discardMetrics.NewGauge(),
		serverActivePlayer:      discardMetrics.NewGauge(),
		serverLogins:            discardMetrics.NewCounter(),
		serverActiveConnections: discardMetrics.NewGauge(),
		rateLimitAvailable:      discardMetrics.NewGauge(),
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
	context.AfterFunc(ctx, func() {
		_ = client.Close()
	})

	go b.metrics.WriteLoop(ctx, ticker.C, client)

	logrus.WithField("addr", influxConfig.Addr).
		Debug("reporting metrics to influxdb")

	logrus.Warn("InfluxDB support within mc-router is going to be removed in the future. Refer to https://github.com/itzg/mc-router/issues/615")

	return nil
}

func (b *influxMetricsBuilder) BuildConnectorMetrics() ConnectorMetrics {
	influxConfig := &b.config.Influxdb

	influxClient := kitinflux.New(influxConfig.Tags, influx.BatchPointsConfig{
		Database:        influxConfig.Database,
		RetentionPolicy: influxConfig.RetentionPolicy,
	}, kitlogrus.NewLogger(logrus.StandardLogger()))

	b.metrics = influxClient

	c := influxClient.NewCounter("mc_router_connections")
	return &goKitConnectorMetrics{
		errorsCounter:           influxClient.NewCounter("mc_router_errors"),
		bytesTransmitted:        influxClient.NewCounter("mc_router_transmitted_bytes"),
		connectionsFrontend:     c.With("side", "frontend"),
		connectionsBackend:      c.With("side", "backend"),
		activeConnections:       influxClient.NewGauge("mc_router_connections_active"),
		serverActivePlayer:      influxClient.NewGauge("mc_router_server_player_active"),
		serverLogins:            influxClient.NewCounter("mc_router_server_logins"),
		serverActiveConnections: influxClient.NewGauge("mc_router_server_active_connections"),
		rateLimitAvailable:      influxClient.NewGauge("mc_router_rate_limit_available"),
	}
}

type prometheusMetricsBuilder struct {
}

var pcv *prometheusMetrics.Counter

func (b prometheusMetricsBuilder) Start(ctx context.Context) error {

	// nothing needed
	return nil
}

func (b prometheusMetricsBuilder) BuildConnectorMetrics() ConnectorMetrics {
	pcv = prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "mc_router",
		Name:      "errors",
		Help:      "The total number of errors",
	}, []string{"type"}))
	return &goKitConnectorMetrics{
		errorsCounter: pcv,
		bytesTransmitted: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mc_router",
			Name:      "bytes",
			Help:      "The total number of bytes transmitted",
		}, nil)),
		connectionsFrontend: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "mc_router",
			Subsystem:   "frontend",
			Name:        "connections",
			Help:        "The total number of connections",
			ConstLabels: prometheus.Labels{"side": "frontend"},
		}, nil)),
		connectionsBackend: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "mc_router",
			Subsystem:   "backend",
			Name:        "connections",
			Help:        "The total number of backend connections",
			ConstLabels: prometheus.Labels{"side": "backend"},
		}, []string{"host"})),
		activeConnections: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "active_connections",
			Help:      "The number of active connections",
		}, nil)),
		serverActivePlayer: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "server_active_player",
			Help:      "Player is active on server",
		}, []string{"player_name", "player_uuid", "server_address"})),
		serverLogins: prometheusMetrics.NewCounter(promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mc_router",
			Name:      "server_logins",
			Help:      "The total number of player logins",
		}, []string{"player_name", "player_uuid", "server_address"})),
		serverActiveConnections: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "server_active_connections",
			Help:      "The number of active connections per server",
		}, []string{"server_address"})),
		rateLimitAvailable: prometheusMetrics.NewGauge(promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mc_router",
			Name:      "rate_limit_available",
			Help:      "The number of available tokens in the rate limit bucket",
		}, nil)),
	}
}
