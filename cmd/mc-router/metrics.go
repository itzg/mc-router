package main

import (
	"context"
	"errors"
	"fmt"
	kitlogrus "github.com/go-kit/kit/log/logrus"
	discardMetrics "github.com/go-kit/kit/metrics/discard"
	expvarMetrics "github.com/go-kit/kit/metrics/expvar"
	kitinflux "github.com/go-kit/kit/metrics/influx"
	influx "github.com/influxdata/influxdb1-client/v2"
	"github.com/itzg/mc-router/server"
	"github.com/sirupsen/logrus"
	"strings"
	"time"
)

type MetricsBuilder interface {
	BuildConnectorMetrics() *server.ConnectorMetrics
	Start(ctx context.Context) error
}

func NewMetricsBuilder(backend string, config *MetricsBackendConfig) MetricsBuilder {
	switch strings.ToLower(backend) {
	case "expvar":
		return &expvarMetricsBuilder{}
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
	return &server.ConnectorMetrics{
		Errors:            expvarMetrics.NewCounter("errors").With("subsystem", "connector"),
		BytesTransmitted:  expvarMetrics.NewCounter("bytes"),
		Connections:       expvarMetrics.NewCounter("connections"),
		ActiveConnections: expvarMetrics.NewGauge("active_connections"),
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
		Errors:            discardMetrics.NewCounter(),
		BytesTransmitted:  discardMetrics.NewCounter(),
		Connections:       discardMetrics.NewCounter(),
		ActiveConnections: discardMetrics.NewGauge(),
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
	}, kitlogrus.NewLogrusLogger(logrus.StandardLogger()))

	b.metrics = metrics

	return &server.ConnectorMetrics{
		Errors:            metrics.NewCounter("errors"),
		BytesTransmitted:  metrics.NewCounter("transmitted_bytes"),
		Connections:       metrics.NewCounter("connections"),
		ActiveConnections: metrics.NewGauge("connections_active"),
	}
}
