package telemetry

import (
	"context"
	"log/slog"
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

func TestSetupNoopWhenUnconfigured(t *testing.T) {
	clearOTELEnv(t)

	shutdown, err := Setup(context.Background(), Config{
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
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned error: %v", err)
	}
}

func TestSetupReturnsShutdownWhenConfigured(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	shutdown, err := Setup(context.Background(), Config{
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
	// Shutdown may attempt a flush; give it a bounded context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}

func TestEndpointConfigured(t *testing.T) {
	clearOTELEnv(t)
	if endpointConfigured() {
		t.Error("expected no endpoint configured")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	if !endpointConfigured() {
		t.Error("expected traces endpoint to be detected")
	}
}

func TestEndpointConfiguredMetrics(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://localhost:4318/v1/metrics")
	if !endpointConfigured() {
		t.Error("expected metrics endpoint to be detected")
	}
}
