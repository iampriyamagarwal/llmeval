// Package logger provides a structured logger built on log/slog.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// ParseLevel converts a textual log level into an slog.Level. Unknown or empty
// values default to slog.LevelInfo.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a *slog.Logger. It uses a JSON handler when env == "production"
// and a text handler otherwise. The level is parsed from logLevel and the env
// is attached as a default attribute on every record.
func New(env, logLevel string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(logLevel)}

	var handler slog.Handler
	if strings.EqualFold(env, "production") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler).With(slog.String("env", env))
	return logger
}
