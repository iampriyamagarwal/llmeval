// Package handlers contains the HTTP handlers, routing, and middleware for the
// service.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	httpx "llmeval/internal/clients"
)

// Config bundles everything a Handler needs. It is passed to New so callers
// (see cmd/server/main.go) can wire dependencies without a long positional
// argument list.
type Config struct {
	// Logger is used for request/error logging. If nil, slog.Default() is used.
	Logger *slog.Logger
	// Env, Service, and Version are surfaced by the root/health endpoints.
	Env     string
	Service string
	Version string
	// InferenceEndpoint is the upstream chat-completions URL that /v1/chat
	// proxies requests to.
	InferenceEndpoint string
	// ModelAccessKey authenticates outbound requests to the inference endpoint.
	ModelAccessKey string
	// Primary is the client whose response is served back to the caller.
	Primary *http.Client
	// Shadow is the client used for mirrored/comparison traffic whose result
	// does not affect the caller-facing response.
	Shadow *http.Client
}

// Handler holds dependencies shared across HTTP handlers.
type Handler struct {
	logger  *slog.Logger
	env     string
	service string
	version string

	inferenceEndpoint string
	modelAccessKey    string

	primary *http.Client
	shadow  *http.Client
}

// New constructs a Handler from cfg. If cfg.Logger is nil the slog default is
// used. The primary and shadow HTTP clients are built and tuned by the caller
// and injected via cfg.
func New(cfg Config) *Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		logger:            logger,
		env:               cfg.Env,
		service:           cfg.Service,
		version:           cfg.Version,
		inferenceEndpoint: cfg.InferenceEndpoint,
		modelAccessKey:    cfg.ModelAccessKey,
		primary:           cfg.Primary,
		shadow:            cfg.Shadow,
	}
}

// Routes builds the *http.ServeMux with all routes registered using Go 1.22+
// method-based routing, wrapped with the request-logging middleware.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	inner := http.NewServeMux()
	// {$} anchors the match to the exact root path so unknown paths 404.
	inner.HandleFunc("GET /{$}", h.root)
	inner.HandleFunc("GET /health", h.health)
	inner.HandleFunc("POST /v1/chat", h.chat)

	mux.Handle("/", h.loggingMiddleware(inner))
	return mux
}

// root handles GET / and returns a welcome message.
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "Welcome to " + h.service,
		"service": h.service,
		"version": "v1.0.1",
	})
}

// health handles GET /health and returns status, env, and the current UTC time.
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"env":    h.env,
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// chat handles POST /v1/chat by proxying the request body to the configured
// inference endpoint using the primary client and streaming the upstream
// response (status + body) back to the caller.
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error("read chat request body", slog.Any("error", err))
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	resp, err := httpx.Send(r.Context(), h.primary, httpx.DefaultRetryConfig(),
		func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.inferenceEndpoint, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			if h.modelAccessKey != "" {
				req.Header.Set("Authorization", "Bearer "+h.modelAccessKey)
			}
			return req, nil
		})
	if err != nil {
		// Retryable failures that exhausted their attempts come back as an
		// *APIError carrying the upstream status/body; forward it. Everything
		// else (transport error, context cancellation) is a bad gateway.
		var apiErr *httpx.APIError
		if errors.As(err, &apiErr) {
			h.logger.Error("inference upstream error",
				slog.Int("status", apiErr.StatusCode), slog.String("body", apiErr.Body))
			writeUpstream(w, apiErr.StatusCode, "application/json", []byte(apiErr.Body))
			return
		}
		h.logger.Error("inference request failed", slog.Any("error", err))
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed"})
		return
	}
	defer httpx.Drain(resp)

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.logger.Error("stream inference response", slog.Any("error", err))
	}
}

// writeUpstream writes a proxied upstream response (status, content-type, body)
// straight back to the caller.
func writeUpstream(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusRecorder wraps http.ResponseWriter to capture the status code written.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware logs method, path, status, latency, and remote_addr for
// every request.
func (h *Handler) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		h.logger.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("latency", time.Since(start)),
			slog.String("remote_addr", r.RemoteAddr),
		)
	})
}
