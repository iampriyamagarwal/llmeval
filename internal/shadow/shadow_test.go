package shadow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	httpx "llmeval/internal/clients"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// setupMeter installs a fresh manual-reader MeterProvider as the global provider
// so a Comparator created afterwards records into instruments we can collect.
func setupMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return reader
}

// counterValue collects and returns the value of the named Int64 sum data point
// matching all wanted attributes (0 if not found).
func counterValue(t *testing.T, r *sdkmetric.ManualReader, name string, want ...attribute.KeyValue) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if attrsMatch(dp.Attributes, want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

func attrsMatch(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		v, ok := set.Value(kv.Key)
		if !ok || v.String() != kv.Value.String() {
			return false
		}
	}
	return true
}

// shadowServer returns a test server that responds with the given status/body
// and records the last request body it received.
func shadowServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Pointer[string]) {
	t.Helper()
	var last atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		last.Store(&s)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func runJob(t *testing.T, c *Comparator, in Input) {
	t.Helper()
	if err := c.Job(in)(context.Background()); err != nil {
		t.Fatalf("shadow job returned error = %v, want nil (best-effort)", err)
	}
}

func TestJobMatchingActionsRecordsSuccessAndMatch(t *testing.T) {
	reader := setupMeter(t)
	srv, _ := shadowServer(t, http.StatusOK, `{"action":[{"type":"move"}],"model":"s"}`)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL, Model: "shadow-model"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{"model":"primary","messages":[]}`),
		PrimaryPayload: []byte(`{"action":[{"type":"move"}],"model":"p"}`),
	})

	if got := counterValue(t, reader, "shadow.requests.total"); got != 1 {
		t.Errorf("requests total = %d, want 1", got)
	}
	if got := counterValue(t, reader, "shadow.success.total"); got != 1 {
		t.Errorf("success total = %d, want 1", got)
	}
	if got := counterValue(t, reader, "shadow.actions.comparisons.total", attribute.Bool("match", true)); got != 1 {
		t.Errorf("match=true comparisons = %d, want 1", got)
	}
}

func TestJobMismatchedActionsRecordsMismatch(t *testing.T) {
	reader := setupMeter(t)
	srv, _ := shadowServer(t, http.StatusOK, `{"action":[{"type":"stop"}]}`)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL, Model: "shadow-model"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{"model":"primary"}`),
		PrimaryPayload: []byte(`{"action":[{"type":"move"}]}`),
	})

	if got := counterValue(t, reader, "shadow.success.total"); got != 1 {
		t.Errorf("success total = %d, want 1", got)
	}
	if got := counterValue(t, reader, "shadow.actions.comparisons.total", attribute.Bool("match", false)); got != 1 {
		t.Errorf("match=false comparisons = %d, want 1", got)
	}
}

func TestJobPrimaryUnparsableSkipsShadowCall(t *testing.T) {
	reader := setupMeter(t)
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{}`),
		PrimaryPayload: []byte(`not json`),
	})

	if got := called.Load(); got != 0 {
		t.Errorf("shadow endpoint called %d times, want 0 (primary unparsable short-circuits)", got)
	}
	if got := counterValue(t, reader, "shadow.failure.total", attribute.String("reason", reasonPrimaryUnparsable)); got != 1 {
		t.Errorf("primary_unparsable failures = %d, want 1", got)
	}
}

func TestJobShadowUnparsableRecordsFailure(t *testing.T) {
	reader := setupMeter(t)
	srv, _ := shadowServer(t, http.StatusOK, `definitely not json`)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{}`),
		PrimaryPayload: []byte(`{"action":[]}`),
	})

	if got := counterValue(t, reader, "shadow.failure.total", attribute.String("reason", reasonShadowUnparsable)); got != 1 {
		t.Errorf("shadow_unparsable failures = %d, want 1", got)
	}
	if got := counterValue(t, reader, "shadow.success.total"); got != 0 {
		t.Errorf("success total = %d, want 0", got)
	}
}

func TestJobShadowStatusErrorRecordsFailure(t *testing.T) {
	reader := setupMeter(t)
	// 400 is non-retryable, so httpx.Send returns it without error.
	srv, _ := shadowServer(t, http.StatusBadRequest, `{"error":"bad"}`)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{}`),
		PrimaryPayload: []byte(`{"action":[]}`),
	})

	if got := counterValue(t, reader, "shadow.failure.total", attribute.String("reason", reasonShadowStatus)); got != 1 {
		t.Errorf("shadow_status failures = %d, want 1", got)
	}
}

func TestJobTimeoutRecordsTimeout(t *testing.T) {
	reader := setupMeter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Timeout = 30 * time.Millisecond

	c, err := New(Config{
		Client:   client,
		Endpoint: srv.URL,
		Retry:    httpx.RetryConfig{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{}`),
		PrimaryPayload: []byte(`{"action":[]}`),
	})

	if got := counterValue(t, reader, "shadow.timeout.total"); got != 1 {
		t.Errorf("timeout total = %d, want 1", got)
	}
	if got := counterValue(t, reader, "shadow.failure.total", attribute.String("reason", reasonTimeout)); got != 1 {
		t.Errorf("timeout failures = %d, want 1", got)
	}
}

func TestJobRewritesModelInShadowRequest(t *testing.T) {
	setupMeter(t)
	srv, last := shadowServer(t, http.StatusOK, `{"action":[]}`)

	c, err := New(Config{Client: srv.Client(), Endpoint: srv.URL, Model: "shadow-model"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runJob(t, c, Input{
		Body:           []byte(`{"model":"primary","messages":[{"role":"user"}]}`),
		PrimaryPayload: []byte(`{"action":[]}`),
	})

	got := last.Load()
	if got == nil {
		t.Fatal("shadow endpoint received no request")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(*got), &sent); err != nil {
		t.Fatalf("shadow body not JSON: %v", err)
	}
	if sent["model"] != "shadow-model" {
		t.Errorf("shadow model = %v, want shadow-model", sent["model"])
	}
	if _, ok := sent["messages"]; !ok {
		t.Error("shadow body lost the messages field during model rewrite")
	}
}

func TestRecordDroppedIncrementsCounter(t *testing.T) {
	reader := setupMeter(t)

	c, err := New(Config{Endpoint: "http://example.invalid"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	c.RecordDropped(context.Background())
	c.RecordDropped(context.Background())

	if got := counterValue(t, reader, "shadow.dropped.total"); got != 2 {
		t.Errorf("dropped total = %d, want 2", got)
	}
}

func TestExtractActions(t *testing.T) {
	if _, ok := extractActions([]byte(`not json`)); ok {
		t.Error("expected ok=false for non-JSON payload")
	}
	if _, ok := extractActions([]byte(`[1,2,3]`)); ok {
		t.Error("expected ok=false for non-object JSON payload")
	}
	actions, ok := extractActions([]byte(`{"other":1}`))
	if !ok || actions != nil {
		t.Errorf("missing actions: got (%v, %v), want (nil, true)", actions, ok)
	}
	if _, ok := extractActions([]byte(`{"action":["a","b"]}`)); !ok {
		t.Error("expected ok=true for object with actions")
	}
}
