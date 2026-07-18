package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
		" info ":  slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewProductionUsesJSON(t *testing.T) {
	logger := New("production", "info")
	if logger == nil {
		t.Fatal("New returned nil")
	}
	h := logger.Handler()
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("production handler = %T, want *slog.JSONHandler", h)
	}
}

func TestNewNonProductionUsesText(t *testing.T) {
	logger := New("development", "info")
	h := logger.Handler()
	if _, ok := h.(*slog.TextHandler); !ok {
		t.Errorf("development handler = %T, want *slog.TextHandler", h)
	}
}

func TestNewAttachesEnvAttribute(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler).With(slog.String("env", "staging"))
	logger.Info("hello")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if record["env"] != "staging" {
		t.Errorf("env attribute = %v, want staging", record["env"])
	}
}

func TestNewRespectsLevel(t *testing.T) {
	logger := New("production", "error")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info level should be disabled when LOG_LEVEL=error")
	}
	if !logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("error level should be enabled when LOG_LEVEL=error")
	}
}

func TestNewDebugLevelEnabled(t *testing.T) {
	logger := New("production", "debug")
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug level should be enabled when LOG_LEVEL=debug")
	}
}
