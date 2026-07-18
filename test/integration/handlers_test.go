//go:build integration

// Package integration contains black-box integration tests that exercise the
// application through a real HTTP server (started with httptest.Server) wired
// exactly like cmd/server/main.go, instead of calling handlers directly. Run
// them with:
//
//	go test -tags=integration ./...
package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	httpx "llmeval/internal/clients"
	"llmeval/internal/handlers"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	testEnv     = "integration"
	testService = "llmeval"
	testVersion = "test-version"
)

// newServer starts an httptest.Server wired exactly like cmd/server/main.go:
// the real router built by handlers.New wrapped with the otelhttp handler. It
// returns the base URL and registers cleanup to close the server.
func newServer(t *testing.T) string {
	return newServerWithUpstream(t, "")
}

// newServerWithUpstream is like newServer but points the chat proxy at the
// given inference endpoint URL.
func newServerWithUpstream(t *testing.T, inferenceEndpoint string) string {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := handlers.New(handlers.Config{
		Logger:            log,
		Env:               testEnv,
		Service:           testService,
		Version:           testVersion,
		InferenceEndpoint: inferenceEndpoint,
		ModelAccessKey:    "integration-key",
		Primary:           httpx.NewClient(httpx.Config{}),
		Shadow:            httpx.NewClient(httpx.Config{}),
	})
	handler := otelhttp.NewHandler(h.Routes(), "http.server")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv.URL
}

// do performs a real HTTP request against the test server and returns the
// status code together with the raw (undecoded) body.
func do(t *testing.T, method, url string) (int, []byte) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body from %s %s: %v", method, url, err)
	}
	return resp.StatusCode, body
}

// getJSON performs a real HTTP GET and decodes the JSON body into a string map.
func getJSON(t *testing.T, url string) (int, map[string]string) {
	t.Helper()
	status, raw := do(t, http.MethodGet, url)
	return status, decode(t, url, raw)
}

func decode(t *testing.T, url string, raw []byte) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body from %s: %v (body=%q)", url, err, raw)
	}
	return body
}

func TestHealthEndpoint(t *testing.T) {
	base := newServer(t)

	status, body := getJSON(t, base+"/health")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want %q", body["status"], "ok")
	}
	if body["env"] != testEnv {
		t.Errorf("env field = %q, want %q", body["env"], testEnv)
	}
	if _, err := time.Parse(time.RFC3339, body["time"]); err != nil {
		t.Errorf("time field %q is not RFC3339: %v", body["time"], err)
	}
}

func TestRootEndpoint(t *testing.T) {
	base := newServer(t)

	status, body := getJSON(t, base+"/")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["message"] == "" {
		t.Errorf("expected a non-empty welcome message, got %q", body["message"])
	}
	if body["service"] != testService {
		t.Errorf("service field = %q, want %q", body["service"], testService)
	}
}

func TestChatEndpoint(t *testing.T) {
	// Fake upstream inference endpoint the proxy forwards to.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer integration-key" {
			t.Errorf("upstream Authorization = %q, want %q", got, "Bearer integration-key")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-xyz","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	base := newServerWithUpstream(t, upstream.URL)

	status, raw := do(t, http.MethodPost, base+"/v1/chat")
	body := decode(t, base+"/v1/chat", raw)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["id"] != "chatcmpl-xyz" {
		t.Errorf("id field = %q, want %q", body["id"], "chatcmpl-xyz")
	}
	if body["object"] != "chat.completion" {
		t.Errorf("object field = %q, want %q", body["object"], "chat.completion")
	}
}

func TestChatWrongMethodReturns405(t *testing.T) {
	base := newServer(t)

	status, _ := do(t, http.MethodGet, base+"/v1/chat")

	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", status, http.StatusMethodNotAllowed)
	}
}

// TestUnknownPathReturns404 exercises the default mux 404, which returns a
// plain-text body (not JSON), so the raw body is asserted directly.
func TestUnknownPathReturns404(t *testing.T) {
	base := newServer(t)

	status, _ := do(t, http.MethodGet, base+"/does-not-exist")

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestConcurrentRequests fires a burst of concurrent requests at the server to
// exercise the shared handler (and its primary/shadow clients) under load; all
// requests should succeed.
func TestConcurrentRequests(t *testing.T) {
	base := newServer(t)

	const n = 24
	codes := make([]int, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(base + "/health")
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d failed: %v", i, errs[i])
		}
		if codes[i] != http.StatusOK {
			t.Errorf("request %d: status = %d, want %d", i, codes[i], http.StatusOK)
		}
	}
}
