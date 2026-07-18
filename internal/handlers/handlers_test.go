package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// newTestServer builds the otelhttp-wrapped router used in production.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := New(slog.Default(), "test", "llmeval", "test-version")
	router := otelhttp.NewHandler(h.Routes(), "http.server")
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestRootRoute(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	decode(t, resp.Body, &body)
	if body["service"] != "llmeval" {
		t.Errorf("service = %v, want llmeval", body["service"])
	}
	if _, ok := body["message"]; !ok {
		t.Error("missing message field")
	}
}

func TestHealthRoute(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body map[string]any
	decode(t, resp.Body, &body)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if body["env"] != "test" {
		t.Errorf("env = %v, want test", body["env"])
	}
	if _, ok := body["time"]; !ok {
		t.Error("missing time field")
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET /does-not-exist: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestStatusRecorderCapturesCode(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusTeapot)
	if rec.status != http.StatusTeapot {
		t.Errorf("recorded status = %d, want %d", rec.status, http.StatusTeapot)
	}
}

func decode(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}
