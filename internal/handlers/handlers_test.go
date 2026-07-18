package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpx "llmeval/internal/clients"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// newTestServer builds the otelhttp-wrapped router used in production, with no
// upstream inference endpoint configured.
func newTestServer(t *testing.T) *httptest.Server {
	return newTestServerWithUpstream(t, "")
}

// newTestServerWithUpstream is like newTestServer but points the chat proxy at
// the given inference endpoint URL.
func newTestServerWithUpstream(t *testing.T, inferenceEndpoint string) *httptest.Server {
	t.Helper()
	h := New(Config{
		Logger:            slog.Default(),
		Env:               "test",
		Service:           "llmeval",
		Version:           "test-version",
		InferenceEndpoint: inferenceEndpoint,
		ModelAccessKey:    "test-key",
		Primary:           httpx.NewClient(httpx.Config{}),
		Shadow:            httpx.NewClient(httpx.Config{}),
	})
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
	defer func() { _ = resp.Body.Close() }()

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
	defer func() { _ = resp.Body.Close() }()

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

func TestChatRoute(t *testing.T) {
	// Fake upstream inference endpoint that records what the proxy forwarded.
	var gotAuth, gotBody, gotContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-abc","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	srv := newTestServerWithUpstream(t, upstream.URL)

	reqBody := `{
		"model": "openai-gpt-5.6-terra",
		"messages": [{"role": "user", "content": "What is the capital of France?"}],
		"max_tokens": 100
	}`

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// The upstream (proxied) response is returned verbatim to the caller.
	var body map[string]any
	decode(t, resp.Body, &body)
	if body["id"] != "chatcmpl-abc" {
		t.Errorf("id = %v, want chatcmpl-abc", body["id"])
	}

	// The proxy forwarded the request body, JSON content type, and the
	// configured model access key as a bearer token.
	if gotAuth != "Bearer test-key" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", gotContentType)
	}
	if !strings.Contains(gotBody, "openai-gpt-5.6-terra") {
		t.Errorf("upstream body = %q, want it to contain the forwarded model", gotBody)
	}
}

func TestChatRouteForwardsUpstreamError(t *testing.T) {
	// Upstream returns a non-retryable 4xx; the proxy must forward it as-is.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer upstream.Close()

	srv := newTestServerWithUpstream(t, upstream.URL)

	resp, err := http.Post(srv.URL+"/v1/chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /v1/chat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	var body map[string]any
	decode(t, resp.Body, &body)
	if body["error"] != "invalid api key" {
		t.Errorf("error = %v, want 'invalid api key'", body["error"])
	}
}

func TestChatRouteWrongMethodReturns405(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/v1/chat")
	if err != nil {
		t.Fatalf("GET /v1/chat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET /does-not-exist: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
