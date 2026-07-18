package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		t.Setenv(key, "")
	}
}

func TestSetupExposesMetricsWhenTracingUnconfigured(t *testing.T) {
	clearOTELEnv(t)

	shutdown, metricsHandler, err := Setup(context.Background(), Config{
		ServiceName:    "llmeval",
		ServiceVersion: "test",
		Env:            "test",
	}, slog.Default())
	if err != nil {
		t.Fatalf("Setup() error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func is nil")
	}
	// Metrics are pull-based and always enabled, so a handler must be returned
	// even when no OTLP tracing endpoint is configured.
	if metricsHandler == nil {
		t.Fatal("metrics handler is nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metricsHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("metrics handler status = %d, want %d", rec.Code, http.StatusOK)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

func TestSetupReturnsShutdownWhenTracingConfigured(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	shutdown, metricsHandler, err := Setup(context.Background(), Config{
		ServiceName:    "llmeval",
		ServiceVersion: "test",
		Env:            "test",
	}, slog.Default())
	if err != nil {
		t.Fatalf("Setup() error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func is nil when endpoint configured")
	}
	if metricsHandler == nil {
		t.Fatal("metrics handler is nil")
	}
	// Shutdown may attempt a flush; give it a bounded context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}

func TestTracesEndpointConfigured(t *testing.T) {
	clearOTELEnv(t)
	if tracesEndpointConfigured() {
		t.Error("expected no traces endpoint configured")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	if !tracesEndpointConfigured() {
		t.Error("expected traces endpoint to be detected")
	}
}

func TestTracesEndpointIgnoresMetricsEndpoint(t *testing.T) {
	clearOTELEnv(t)
	// A metrics-only OTLP endpoint must not enable OTLP tracing; metrics are
	// scraped, not pushed.
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://localhost:4318/v1/metrics")
	if tracesEndpointConfigured() {
		t.Error("metrics endpoint should not enable tracing")
	}
}
