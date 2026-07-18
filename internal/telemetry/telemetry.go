// Package telemetry wires up OpenTelemetry tracing and metrics.
//
// Tracing is driven by the standard OTEL_* environment variables and pushed to
// an OTLP/HTTP endpoint when one is configured; otherwise a no-op tracer is
// installed and no network connections are made.
//
// Metrics are exposed on a Prometheus-compatible endpoint (returned as an
// http.Handler that callers mount at /metrics) for an external collector to
// scrape. No metric data is pushed anywhere.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config carries the resource attributes reported to the telemetry backend.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Env            string
}

// ShutdownFunc flushes and releases telemetry resources.
type ShutdownFunc func(context.Context) error

// noopShutdown is a shutdown function that does nothing.
func noopShutdown(context.Context) error { return nil }

// tracesEndpointConfigured reports whether an OTLP traces endpoint is set via
// the standard OTEL_* environment variables (general or traces-specific).
func tracesEndpointConfigured() bool {
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// Setup initialises OpenTelemetry and returns a shutdown function together with
// an http.Handler that serves metrics in the Prometheus exposition format. The
// caller is expected to mount that handler at /metrics.
//
// Metrics use a pull-based Prometheus exporter, so they make no outbound
// network connections and are always enabled. Tracing pushes over OTLP/HTTP
// only when an endpoint is configured; otherwise a no-op tracer is installed.
// A component that fails to build downgrades to no-op with a warning rather
// than erroring, so this function never crashes the app. The returned metrics
// handler is nil only when the Prometheus exporter itself fails to build.
func Setup(ctx context.Context, cfg Config, log *slog.Logger) (ShutdownFunc, http.Handler, error) {
	if log == nil {
		log = slog.Default()
	}

	// Composite W3C tracecontext + baggage propagator. Cheap and makes no
	// network connections, so it is always installed.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		// deployment.environment.name (raw key for cross-version compatibility).
		attribute.String("deployment.environment.name", cfg.Env),
	))
	if err != nil {
		log.Warn("telemetry: failed to build resource, falling back to default", slog.Any("error", err))
		res = resource.Default()
	}

	// shutdowns are invoked in order to flush/release each configured provider.
	var shutdowns []ShutdownFunc

	// Metrics: pull-based Prometheus exporter exposed via the returned handler.
	var metricsHandler http.Handler
	registry := prometheus.NewRegistry()
	promExp, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		log.Warn("telemetry: failed to build Prometheus metric exporter, metrics disabled", slog.Any("error", err))
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
	} else {
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(promExp),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
		metricsHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

		if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
			log.Warn("telemetry: failed to start runtime metrics", slog.Any("error", err))
		}
		log.Info("telemetry: metrics exposed for Prometheus scraping at /metrics",
			slog.String("service.name", cfg.ServiceName),
			slog.String("service.version", cfg.ServiceVersion),
			slog.String("deployment.environment.name", cfg.Env),
		)
	}

	// Traces: OTLP/HTTP push when an endpoint is configured, else no-op.
	if tracesEndpointConfigured() {
		traceExp, err := otlptracehttp.New(ctx)
		if err != nil {
			log.Warn("telemetry: failed to build trace exporter, tracing disabled", slog.Any("error", err))
			otel.SetTracerProvider(tracenoop.NewTracerProvider())
		} else {
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(traceExp),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(tp)
			shutdowns = append(shutdowns, tp.Shutdown)
			log.Info("telemetry: OTLP/HTTP trace exporter configured")
		}
	} else {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		log.Info("telemetry: no OTLP traces endpoint configured, tracing disabled")
	}

	if len(shutdowns) == 0 {
		return noopShutdown, metricsHandler, nil
	}

	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdowns {
			if e := fn(ctx); e != nil {
				err = e
			}
		}
		return err
	}

	return shutdown, metricsHandler, nil
}
