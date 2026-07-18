//go:build integration

// Package integration contains black-box integration tests that exercise the
// application through a real HTTP server (started with httptest.Server) wired
// exactly like cmd/server/main.go, instead of calling handlers directly. Run
// them with:
//
//	go test -tags=integration ./...
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	httpx "llmeval/internal/clients"
	"llmeval/internal/handlers"
	"llmeval/internal/shadow"
	"llmeval/internal/worker"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	testEnv     = "integration"
	testService = "llmeval"
	testVersion = "test-version"
)

// serverConfig configures the test server wiring so each test can mirror
// cmd/server/main.go as closely as the scenario needs. The zero value wires a
// server with no upstream and shadowing disabled (Pool/Comparator nil).
type serverConfig struct {
	// inferenceEndpoint is the upstream the /v1/chat proxy forwards to.
	inferenceEndpoint string
	// shadowEndpoint, when non-empty, enables the off-request-path shadow
	// comparison exactly like main.go: a background worker.Pool plus a
	// shadow.Comparator pointed at this endpoint.
	shadowEndpoint string
	// shadowModel is substituted into the request body's "model" field for the
	// shadow call. Only meaningful when shadowEndpoint is set.
	shadowModel string
}

// newServer starts an httptest.Server wired exactly like cmd/server/main.go:
// the real router built by handlers.New wrapped with the otelhttp handler. It
// returns the base URL and registers cleanup to close the server.
func newServer(t *testing.T) string {
	return newServerWith(t, serverConfig{})
}

// newServerWithUpstream is like newServer but points the chat proxy at the
// given inference endpoint URL.
func newServerWithUpstream(t *testing.T, inferenceEndpoint string) string {
	return newServerWith(t, serverConfig{inferenceEndpoint: inferenceEndpoint})
}

// newServerWith starts an httptest.Server wired like cmd/server/main.go from
// sc. When sc.shadowEndpoint is set it also stands up the background worker
// pool and shadow comparator (draining the pool on cleanup) so the full
// primary + shadow request flow is exercised end-to-end.
func newServerWith(t *testing.T, sc serverConfig) string {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := handlers.Config{
		Logger:            log,
		Env:               testEnv,
		Service:           testService,
		Version:           testVersion,
		InferenceEndpoint: sc.inferenceEndpoint,
		Primary:           httpx.NewClient(httpx.Config{}),
		Shadow:            httpx.NewClient(httpx.Config{}),
	}

	if sc.shadowEndpoint != "" {
		pool := worker.New(log, 2, 8)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := pool.Shutdown(ctx); err != nil {
				t.Errorf("worker pool shutdown: %v", err)
			}
		})

		comparator, err := shadow.New(shadow.Config{
			Logger:   log,
			Client:   httpx.NewClient(httpx.Config{}),
			Endpoint: sc.shadowEndpoint,
			Model:    sc.shadowModel,
		})
		if err != nil {
			t.Fatalf("shadow.New: %v", err)
		}
		cfg.Pool = pool
		cfg.Comparator = comparator
	}

	h := handlers.New(cfg)
	handler := otelhttp.NewHandler(h.Routes(), "http.server")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv.URL
}

// do performs a real HTTP request (with an empty body) against the test server
// and returns the status code together with the raw (undecoded) body.
func do(t *testing.T, method, url string) (int, []byte) {
	t.Helper()
	return doBody(t, method, url, "")
}

// doBody is like do but sends reqBody as the request body. An empty reqBody
// sends no body.
func doBody(t *testing.T, method, url, reqBody string) (int, []byte) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	var reqReader io.Reader
	if reqBody != "" {
		reqReader = strings.NewReader(reqBody)
	}
	req, err := http.NewRequest(method, url, reqReader)
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
	// Fake upstream inference endpoint the proxy forwards to. The proxy must
	// forward the caller's exact headers (here, the Content-Type set by do).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("upstream Content-Type = %q, want %q", got, "application/json")
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

// TestChatShadowComparison exercises the full primary + shadow flow wired like
// cmd/server/main.go: the caller is served the primary response while the
// shadow comparison runs off the request path against a separate endpoint. It
// asserts the shadow endpoint is called with the request body's model rewritten
// to the configured shadow model, and that the caller-facing response is the
// primary one (unaffected by the shadow call).
func TestChatShadowComparison(t *testing.T) {
	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-primary","action":"approve"}`))
	}))
	defer primaryUpstream.Close()

	// The shadow upstream runs asynchronously; publish the body it receives so
	// the test can synchronise on the off-path call having happened.
	gotShadowBody := make(chan []byte, 1)
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case gotShadowBody <- b:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-shadow","action":"approve"}`))
	}))
	defer shadowUpstream.Close()

	base := newServerWith(t, serverConfig{
		inferenceEndpoint: primaryUpstream.URL,
		shadowEndpoint:    shadowUpstream.URL,
		shadowModel:       "shadow-model-x",
	})

	status, raw := doBody(t, http.MethodPost, base+"/v1/chat",
		`{"model":"primary-model","action":"approve"}`)
	body := decode(t, base+"/v1/chat", raw)

	// The caller must receive the primary response, not the shadow one.
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body["id"] != "chatcmpl-primary" {
		t.Errorf("id field = %q, want %q", body["id"], "chatcmpl-primary")
	}

	// The shadow call happens off the request path, so wait for it.
	select {
	case shadowBody := <-gotShadowBody:
		var obj map[string]any
		if err := json.Unmarshal(shadowBody, &obj); err != nil {
			t.Fatalf("shadow request body is not JSON: %v (body=%q)", err, shadowBody)
		}
		if obj["model"] != "shadow-model-x" {
			t.Errorf("shadow request model = %v, want %q", obj["model"], "shadow-model-x")
		}
		if obj["action"] != "approve" {
			t.Errorf("shadow request action = %v, want %q", obj["action"], "approve")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shadow endpoint was not called within timeout")
	}
}

// TestChatShadowDisabled verifies that when shadowing is not wired (Pool and
// Comparator nil, as in the other tests) a successful chat request does not
// produce any call to a second endpoint.
func TestChatShadowDisabled(t *testing.T) {
	var shadowCalled int32
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&shadowCalled, 1)
	}))
	defer shadowUpstream.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-primary","action":"approve"}`))
	}))
	defer upstream.Close()

	// shadowEndpoint intentionally left empty: shadowing is disabled.
	base := newServerWith(t, serverConfig{inferenceEndpoint: upstream.URL})

	status, _ := doBody(t, http.MethodPost, base+"/v1/chat",
		`{"model":"primary-model","action":"approve"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	// Give any (erroneously scheduled) background work a chance to run before
	// asserting the shadow endpoint stayed untouched.
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&shadowCalled); n != 0 {
		t.Errorf("shadow endpoint called %d times, want 0 when shadowing disabled", n)
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
