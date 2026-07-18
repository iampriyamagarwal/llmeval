// Package handlers contains the HTTP handlers, routing, and middleware for the
// service.
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Handler holds dependencies shared across HTTP handlers.
type Handler struct {
	logger  *slog.Logger
	env     string
	service string
	version string
}

// New constructs a Handler. If logger is nil the slog default is used.
func New(logger *slog.Logger, env, service, version string) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{logger: logger, env: env, service: service, version: version}
}

// Routes builds the *http.ServeMux with all routes registered using Go 1.22+
// method-based routing, wrapped with the request-logging middleware.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	inner := http.NewServeMux()
	// {$} anchors the match to the exact root path so unknown paths 404.
	inner.HandleFunc("GET /{$}", h.root)
	inner.HandleFunc("GET /health", h.health)

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
