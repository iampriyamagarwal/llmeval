package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newReq is a RequestFunc helper that always targets the given URL with GET.
func newReq(url string) RequestFunc {
	return func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}
}

// fastRetry keeps backoff tiny so retry tests stay quick.
func fastRetry(attempts int) RetryConfig {
	return RetryConfig{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestNewClientHasTimeout(t *testing.T) {
	c := NewClient(Config{})
	if c.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}
	if NewClient(Config{Timeout: 2 * time.Second}).Timeout != 2*time.Second {
		t.Errorf("explicit timeout not honoured")
	}
	if c.Transport == nil {
		t.Error("Transport is nil; expected an instrumented transport")
	}
}

func TestSendReturnsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := Send(context.Background(), srv.Client(), DefaultRetryConfig(), newReq(srv.URL))
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := DecodeJSON(resp, &body); err != nil {
		t.Fatalf("DecodeJSON() error = %v", err)
	}
	if !body.OK {
		t.Error("expected ok=true in decoded body")
	}
}

func TestSendRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := Send(context.Background(), srv.Client(), fastRetry(3), newReq(srv.URL))
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	Drain(resp)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("upstream calls = %d, want 3", got)
	}
}

func TestSendExhaustsRetriesReturnsAPIError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := Send(context.Background(), srv.Client(), fastRetry(3), newReq(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadGateway)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (one per attempt)", got)
	}
}

func TestSendDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	resp, err := Send(context.Background(), srv.Client(), fastRetry(3), newReq(srv.URL))
	if err != nil {
		t.Fatalf("Send() error = %v (4xx should be returned, not retried)", err)
	}
	// The caller surfaces the 4xx via DecodeJSON/CheckResponse.
	err = CheckResponse(resp)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("CheckResponse err = %v, want APIError 400", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestSendHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := Send(ctx, srv.Client(), fastRetry(5), newReq(srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDecodeJSONNonSuccessReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"missing"}`))
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	var out map[string]string
	err = DecodeJSON(resp, &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
	if apiErr.Retryable() {
		t.Error("404 should not be retryable")
	}
}
