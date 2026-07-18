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
	"llmeval/internal/telemetry"
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

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Config{
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
	shadow := httpx.NewClient(httpx.Config{
		Timeout:             cfg.ShadowTimeout,
		MaxIdleConns:        cfg.ShadowMaxIdleConns,
		MaxIdleConnsPerHost: cfg.ShadowMaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.ShadowIdleConnTimeout,
	})

	h := handlers.New(handlers.Config{
		Logger:            log,
		Env:               cfg.AppEnv,
		Service:           cfg.ServiceName,
		Version:           cfg.ServiceVersion,
		InferenceEndpoint: cfg.InferenceEndpoint,
		ModelAccessKey:    cfg.ModelAccessKey,
		Primary:           primary,
		Shadow:            shadow,
	})

	// Wrap the router so every request gets a server span plus the standard
	// HTTP server metrics, alongside the request-logging middleware.
	router := otelhttp.NewHandler(h.Routes(), "http.server")

	srv := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
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

	// Flush telemetry within the same shutdown window.
	if err := shutdownTelemetry(shutdownCtx); err != nil {
		log.Error("telemetry shutdown failed", slog.Any("error", err))
	}

	log.Info("server stopped")
	return nil
}
