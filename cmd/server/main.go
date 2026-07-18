// Command server is the entrypoint for the llmeval REST API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"llmeval/config"
	httpx "llmeval/internal/clients"
	"llmeval/internal/handlers"
	"llmeval/internal/logger"
	"llmeval/internal/shadow"
	"llmeval/internal/telemetry"
	"llmeval/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(cfg.AppEnv, cfg.LogLevel)
	slog.SetDefault(log)

	// Context cancelled on SIGINT/SIGTERM drives graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, metricsHandler, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		Env:            cfg.AppEnv,
	}, log)
	if err != nil {
		return err
	}

	// Build the two outbound HTTP clients with their respective tuning, then
	// inject them into the handler.
	primary := httpx.NewClient(httpx.Config{
		Timeout:             cfg.PrimaryTimeout,
		MaxIdleConns:        cfg.PrimaryMaxIdleConns,
		MaxIdleConnsPerHost: cfg.PrimaryMaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.PrimaryIdleConnTimeout,
	})
	shadowClient := httpx.NewClient(httpx.Config{
		Timeout:             cfg.ShadowTimeout,
		MaxIdleConns:        cfg.ShadowMaxIdleConns,
		MaxIdleConnsPerHost: cfg.ShadowMaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.ShadowIdleConnTimeout,
	})

	// Background pool + comparator that run shadow comparisons off the request
	// path. The shadow endpoint defaults to the primary inference endpoint.
	pool := worker.New(log, cfg.WorkerCount, cfg.WorkerQueueSize)

	shadowEndpoint := cfg.ShadowEndpoint
	if shadowEndpoint == "" {
		shadowEndpoint = cfg.InferenceEndpoint
	}
	comparator, err := shadow.New(shadow.Config{
		Logger:   log,
		Client:   shadowClient,
		Endpoint: shadowEndpoint,
		Model:    cfg.ShadowModel,
	})
	if err != nil {
		return err
	}

	h := handlers.New(handlers.Config{
		Logger:            log,
		Env:               cfg.AppEnv,
		Service:           cfg.ServiceName,
		Version:           cfg.ServiceVersion,
		InferenceEndpoint: cfg.InferenceEndpoint,
		Primary:           primary,
		Shadow:            shadowClient,
		Pool:              pool,
		Comparator:        comparator,
	})

	// Wrap the router so every request gets a server span plus the standard
	// HTTP server metrics, alongside the request-logging middleware.
	router := otelhttp.NewHandler(h.Routes(), "http.server")

	// Serve the Prometheus scrape endpoint outside the traced/logged router so
	// collector scrapes don't generate spans or request-log noise.
	var handler http.Handler = router
	if metricsHandler != nil {
		root := http.NewServeMux()
		root.Handle("GET /metrics", metricsHandler)
		root.Handle("/", router)
		handler = root
	}

	srv := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      handler,
		ReadTimeout:  cfg.ServerReadTimeout,
		WriteTimeout: cfg.ServerWriteTimeout,
		IdleTimeout:  cfg.ServerIdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("starting server",
			slog.String("addr", cfg.Addr()),
			slog.String("service", cfg.ServiceName),
			slog.String("version", cfg.ServiceVersion),
			slog.String("env", cfg.AppEnv),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, shutting down gracefully")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", slog.Any("error", err))
		_ = srv.Close()
	}

	// Drain in-flight/queued shadow comparisons within their own deadline
	// before flushing telemetry so their spans and metrics are exported.
	poolCtx, poolCancel := context.WithTimeout(context.Background(), cfg.WorkerShutdownTimeout)
	defer poolCancel()
	if err := pool.Shutdown(poolCtx); err != nil {
		log.Error("worker pool shutdown failed", slog.Any("error", err))
	}

	// Flush telemetry within the same shutdown window.
	if err := shutdownTelemetry(shutdownCtx); err != nil {
		log.Error("telemetry shutdown failed", slog.Any("error", err))
	}

	log.Info("server stopped")
	return nil
}
