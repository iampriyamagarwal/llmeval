// Package httpx provides shared building blocks for outbound HTTP clients that
// call external APIs. Per-service clients (e.g. internal/clients/placeholder)
// build on these instead of using http.DefaultClient directly, so every
// integration gets the same defaults:
//
//   - a *http.Client with a real timeout and tuned connection pooling,
//   - an OpenTelemetry-instrumented transport (client spans + trace propagation),
//   - retry-with-backoff for transient failures on repeatable requests, and
//   - typed JSON/error decoding via APIError.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Defaults applied by NewClient for any zero-valued Config field. They match
// the previous hard-coded values so callers that don't tune anything keep the
// same behaviour.
const (
	// DefaultTimeout bounds an entire request (connect + write + read) when the
	// caller does not supply one. http.DefaultClient has no timeout and can hang
	// a goroutine forever, which is exactly what we avoid here.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxIdleConns caps idle (keep-alive) connections across all hosts.
	DefaultMaxIdleConns = 100
	// DefaultMaxIdleConnsPerHost caps idle connections per destination host.
	DefaultMaxIdleConnsPerHost = 10
	// DefaultIdleConnTimeout is how long an idle connection is kept before it is
	// closed.
	DefaultIdleConnTimeout = 90 * time.Second
)

// maxErrorBodyBytes caps how much of an error response we read into APIError.
// A few KiB is plenty for logging/debugging without risking a huge allocation.
const maxErrorBodyBytes = 4 << 10

// Config tunes the *http.Client produced by NewClient: the overall request
// timeout plus the underlying transport's connection-pool settings. Any field
// left at its zero value falls back to the matching Default* constant, so a
// zero Config yields the previous built-in defaults.
type Config struct {
	// Timeout bounds an entire request (connect + write + read).
	Timeout time.Duration
	// MaxIdleConns caps idle (keep-alive) connections across all hosts.
	MaxIdleConns int
	// MaxIdleConnsPerHost caps idle connections per destination host.
	MaxIdleConnsPerHost int
	// IdleConnTimeout is how long an idle connection is kept before closing.
	IdleConnTimeout time.Duration
}

// NewClient returns an *http.Client suitable for calling external APIs. Unlike
// http.DefaultClient it always has a timeout, tuned connection pooling, and an
// OpenTelemetry-instrumented transport so outbound calls produce client spans
// and propagate W3C trace context. Zero-valued cfg fields fall back to the
// Default* constants.
func NewClient(cfg Config) *http.Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = DefaultMaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = DefaultIdleConnTimeout
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   cfg.Timeout,
		Transport: otelhttp.NewTransport(transport),
	}
}

// APIError represents a non-2xx response from an external API. Callers can use
// errors.As to inspect the status code and the (truncated) upstream body.
type APIError struct {
	// StatusCode is the HTTP status, e.g. 404 or 503.
	StatusCode int
	// Status is the HTTP status line, e.g. "404 Not Found".
	Status string
	// Body is a truncated copy of the response body for logging/debugging.
	Body string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("external api error: status %d: %s", e.StatusCode, e.Body)
}

// Retryable reports whether a failed request is worth retrying: 429 and 5xx are
// transient, other 4xx are caller errors and must not be retried.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// RetryConfig controls retry-with-backoff behaviour in Send.
type RetryConfig struct {
	// MaxAttempts is the total number of tries (not extra retries). Values < 1
	// are treated as 1 (no retries).
	MaxAttempts int
	// BaseDelay is the first backoff delay; it grows exponentially per attempt.
	BaseDelay time.Duration
	// MaxDelay caps any single backoff delay. Zero means uncapped.
	MaxDelay time.Duration
}

// DefaultRetryConfig is a sensible starting point: 3 attempts with jittered
// exponential backoff between ~100ms and 2s.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
	}
}

// RequestFunc builds a fresh *http.Request for each attempt so retries always
// have a readable body and a live context. It must use the supplied ctx.
type RequestFunc func(ctx context.Context) (*http.Request, error)

// Send executes the request produced by build, retrying transient failures
// (connection errors, 429, and 5xx) per rc with jittered exponential backoff.
// It honours ctx cancellation and any Retry-After header.
//
// On success it returns the *http.Response with its body still open — the
// caller must consume it (e.g. via DecodeJSON, which also drains and closes it).
// Non-retryable non-2xx responses (most 4xx) are returned without error so the
// caller can inspect them with CheckResponse/DecodeJSON.
func Send(ctx context.Context, client *http.Client, rc RetryConfig, build RequestFunc) (*http.Response, error) {
	if rc.MaxAttempts < 1 {
		rc.MaxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= rc.MaxAttempts; attempt++ {
		req, err := build(ctx)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}

		resp, err := client.Do(req)
		switch {
		case err != nil:
			// Transport-level failure (timeout, connection refused, DNS, ...).
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			retryAfter := parseRetryAfter(resp)
			apiErr := readAPIError(resp) // reads + closes the body
			lastErr = apiErr
			if attempt < rc.MaxAttempts {
				if !sleep(ctx, backoff(attempt, rc, retryAfter)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, apiErr
		default:
			// 2xx or a non-retryable status (e.g. 4xx): hand back to the caller.
			return resp, nil
		}

		if attempt < rc.MaxAttempts {
			if !sleep(ctx, backoff(attempt, rc, 0)) {
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastErr
}

// CheckResponse returns an *APIError when resp has a non-2xx status, draining
// and closing the body. On success it returns nil and leaves the body open for
// the caller to read.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return readAPIError(resp)
}

// DecodeJSON validates the status (returning *APIError on non-2xx) and otherwise
// decodes the JSON body into v. The body is always drained and closed so the
// underlying connection can be reused. A nil v checks the status only.
func DecodeJSON(resp *http.Response, v any) error {
	defer Drain(resp)

	if err := CheckResponse(resp); err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}

// Drain reads any remaining bytes and closes the body so the underlying
// connection can return to the pool for reuse. It is safe to call more than
// once and on a nil response/body.
func Drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// readAPIError builds an *APIError from a non-2xx response, reading a truncated
// copy of the body and then draining/closing it.
func readAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	Drain(resp)
	return &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       string(body),
	}
}

// backoff computes the delay before the next attempt. A positive retryAfter
// (from a Retry-After header) takes precedence; otherwise it is exponential in
// the attempt number with full jitter, capped at rc.MaxDelay.
func backoff(attempt int, rc RetryConfig, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return capDelay(retryAfter, rc.MaxDelay)
	}
	base := rc.BaseDelay
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := float64(base) * math.Pow(2, float64(attempt-1))
	d *= 0.5 + rand.Float64()*0.5 // full jitter in [0.5, 1.0]
	return capDelay(time.Duration(d), rc.MaxDelay)
}

func capDelay(d, max time.Duration) time.Duration {
	if max > 0 && d > max {
		return max
	}
	return d
}

// sleep waits for d or until ctx is done. It reports false if ctx was cancelled
// (so the caller should stop retrying).
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseRetryAfter interprets a Retry-After header in either delay-seconds or
// HTTP-date form. It returns 0 when absent or unparseable.
func parseRetryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
