// Package telemetry wires up OpenTelemetry tracing and metrics. It is driven
// entirely by the standard OTEL_* environment variables. When no OTLP endpoint
// is configured it installs no-op providers and makes no network connections.
package telemetry

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
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

// endpointConfigured reports whether any OTLP endpoint is configured via the
// standard OTEL_* environment variables (general or signal-specific).
func endpointConfigured() bool {
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// installNoop sets global no-op providers and a composite propagator so callers
// can use the OpenTelemetry API safely without any exporters configured.
func installNoop() {
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// Setup initialises OpenTelemetry. When no OTLP endpoint is configured it
// installs no-op providers, makes no network connections, and returns a no-op
// shutdown. When configured it installs OTLP/HTTP exporters. An exporter that
// fails to build downgrades to no-op with a warning rather than erroring, so
// this function never crashes the app.
func Setup(ctx context.Context, cfg Config, log *slog.Logger) (ShutdownFunc, error) {
	if log == nil {
		log = slog.Default()
	}

	if !endpointConfigured() {
		installNoop()
		log.Info("telemetry disabled: no OTLP endpoint configured, using no-op providers")
		return noopShutdown, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		// deployment.environment.name (raw key for cross-version compatibility).
		attribute.String("deployment.environment.name", cfg.Env),
	))
	if err != nil {
		log.Warn("telemetry: failed to build resource, downgrading to no-op", slog.Any("error", err))
		installNoop()
		return noopShutdown, nil
	}

	// Composite W3C tracecontext + baggage propagator.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Warn("telemetry: failed to build trace exporter, downgrading to no-op", slog.Any("error", err))
		installNoop()
		return noopShutdown, nil
	}

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		log.Warn("telemetry: failed to build metric exporter, downgrading to no-op", slog.Any("error", err))
		_ = traceExp.Shutdown(ctx)
		installNoop()
		return noopShutdown, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		log.Warn("telemetry: failed to start runtime metrics", slog.Any("error", err))
	}

	log.Info("telemetry enabled: OTLP/HTTP exporters configured",
		slog.String("service.name", cfg.ServiceName),
		slog.String("service.version", cfg.ServiceVersion),
		slog.String("deployment.environment.name", cfg.Env),
	)

	shutdown := func(ctx context.Context) error {
		var err error
		if e := tp.Shutdown(ctx); e != nil {
			err = e
		}
		if e := mp.Shutdown(ctx); e != nil {
			err = e
		}
		return err
	}

	return shutdown, nil
}
