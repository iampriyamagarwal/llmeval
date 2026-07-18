// Package shadow compares the primary model's response against a second
// ("shadow") model off the request path. A comparison is packaged as a
// worker.Job so it runs on the background pool: the caller's request is already
// answered by the primary model, and the shadow call plus comparison happen
// asynchronously, best-effort.
//
// Each comparison answers two questions and records telemetry for both:
//
//  1. Did both models return JSON-parsable payloads?
//  2. Do the "action" keys extracted from each payload match exactly?
//
// Failures (timeouts, transport/status errors, unparsable payloads) and the
// match/mismatch outcome are recorded as OpenTelemetry metrics and span
// attributes so the shadow programme can be monitored without affecting the
// caller-facing path.
package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"

	httpx "llmeval/internal/clients"
	"llmeval/internal/worker"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	// tracerName / meterName are the instrumentation scopes for shadow spans
	// and metrics.
	tracerName = "llmeval/internal/shadow"
	meterName  = "llmeval/internal/shadow"

	// maxPayloadBytes caps how much of the shadow response we buffer for
	// parsing, protecting against an unexpectedly huge upstream body.
	maxPayloadBytes = 1 << 20 // 1 MiB
)

// Failure reasons recorded on the shadow.failure.total counter.
const (
	reasonTimeout           = "timeout"
	reasonRequestError      = "request_error"
	reasonShadowStatus      = "shadow_status"
	reasonPrimaryUnparsable = "primary_unparsable"
	reasonShadowUnparsable  = "shadow_unparsable"
)

// Config configures a Comparator.
type Config struct {
	// Logger is used for warn/error logging. If nil, slog.Default() is used.
	Logger *slog.Logger
	// Client is the HTTP client used for the shadow call. It should be the
	// dedicated shadow client so its traffic is isolated from the primary.
	Client *http.Client
	// Endpoint is the chat-completions URL the shadow request is sent to.
	Endpoint string
	// Model, when non-empty, replaces the top-level "model" field in the
	// request body so the shadow call targets a different model.
	Model string
	// Retry tunes retry-with-backoff for the shadow call. A zero value falls
	// back to httpx.DefaultRetryConfig.
	Retry httpx.RetryConfig
}

// Comparator builds and runs shadow-comparison jobs.
type Comparator struct {
	logger   *slog.Logger
	client   *http.Client
	endpoint string
	model    string
	retry    httpx.RetryConfig

	tracer trace.Tracer

	requests    metric.Int64Counter
	successes   metric.Int64Counter
	failures    metric.Int64Counter
	timeouts    metric.Int64Counter
	comparisons metric.Int64Counter
	dropped     metric.Int64Counter
}

// New creates a Comparator and registers its OpenTelemetry instruments. It
// returns an error only if an instrument fails to register.
func New(cfg Config) (*Comparator, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	retry := cfg.Retry
	if retry.MaxAttempts < 1 {
		retry = httpx.DefaultRetryConfig()
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}

	meter := otel.Meter(meterName)
	requests, err := meter.Int64Counter("shadow.requests.total",
		metric.WithDescription("Total chat requests shadowed against the comparison model."))
	if err != nil {
		return nil, err
	}
	successes, err := meter.Int64Counter("shadow.success.total",
		metric.WithDescription("Shadow calls that returned a JSON-parsable payload and were compared."))
	if err != nil {
		return nil, err
	}
	failures, err := meter.Int64Counter("shadow.failure.total",
		metric.WithDescription("Shadow comparisons that failed, labelled by reason."))
	if err != nil {
		return nil, err
	}
	timeouts, err := meter.Int64Counter("shadow.timeout.total",
		metric.WithDescription("Shadow requests that failed because a deadline/timeout was exceeded."))
	if err != nil {
		return nil, err
	}
	comparisons, err := meter.Int64Counter("shadow.actions.comparisons.total",
		metric.WithDescription("Action-key comparisons between primary and shadow, labelled by match=true|false."))
	if err != nil {
		return nil, err
	}
	dropped, err := meter.Int64Counter("shadow.dropped.total",
		metric.WithDescription("Shadow comparisons dropped without running because the worker queue was full."))
	if err != nil {
		return nil, err
	}

	return &Comparator{
		logger:      logger,
		client:      client,
		endpoint:    cfg.Endpoint,
		model:       cfg.Model,
		retry:       retry,
		tracer:      otel.Tracer(tracerName),
		requests:    requests,
		successes:   successes,
		failures:    failures,
		timeouts:    timeouts,
		comparisons: comparisons,
		dropped:     dropped,
	}, nil
}

// RecordDropped records that a shadow comparison was shed without running
// because the worker queue was full. Callers use this when Pool.Submit returns
// false so load shedding is observable via telemetry.
func (c *Comparator) RecordDropped(ctx context.Context) {
	c.dropped.Add(ctx, 1)
}

// Input is the immutable data a shadow job needs, captured off the request path
// so the job can safely run after the HTTP handler has returned.
type Input struct {
	// Body is the original chat request body forwarded to the primary model.
	Body []byte
	// Header is a clone of the caller's request headers, reused for the shadow
	// call so auth and content-type match the primary request.
	Header http.Header
	// PrimaryPayload is the raw response body the primary model returned.
	PrimaryPayload []byte
}

// Job returns a worker.Job that runs the shadow comparison for in. The job is
// best-effort: it never returns an error, so a comparison failure is recorded
// as telemetry rather than logged by the pool as a failed job.
func (c *Comparator) Job(in Input) worker.Job {
	return func(ctx context.Context) error {
		c.run(ctx, in)
		return nil
	}
}

// run executes a single shadow comparison and records its telemetry.
func (c *Comparator) run(ctx context.Context, in Input) {
	ctx, span := c.tracer.Start(ctx, "shadow.compare")
	defer span.End()

	c.requests.Add(ctx, 1)

	// Question 1a: is the primary payload we captured JSON-parsable?
	primaryActions, ok := extractActions(in.PrimaryPayload)
	if !ok {
		c.fail(ctx, span, reasonPrimaryUnparsable, errors.New("primary payload is not a JSON object"))
		return
	}

	// Call the shadow model with the same request (model field swapped).
	resp, err := httpx.Send(ctx, c.client, c.retry, c.buildRequest(in))
	if err != nil {
		if isTimeout(err) {
			c.timeouts.Add(ctx, 1)
			c.fail(ctx, span, reasonTimeout, err)
			return
		}
		c.fail(ctx, span, reasonRequestError, err)
		return
	}
	defer httpx.Drain(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.fail(ctx, span, reasonShadowStatus, &httpx.APIError{StatusCode: resp.StatusCode, Status: resp.Status})
		return
	}

	shadowBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes))
	if err != nil {
		c.fail(ctx, span, reasonRequestError, err)
		return
	}

	// Question 1b: is the shadow payload JSON-parsable?
	shadowActions, ok := extractActions(shadowBody)
	if !ok {
		c.fail(ctx, span, reasonShadowUnparsable, errors.New("shadow payload is not a JSON object"))
		return
	}

	// Both payloads parsed and were compared: count as a successful shadow.
	c.successes.Add(ctx, 1)

	// Question 2: do the extracted "action" keys match exactly?
	match := reflect.DeepEqual(primaryActions, shadowActions)
	c.comparisons.Add(ctx, 1, metric.WithAttributes(attribute.Bool("match", match)))
	span.SetAttributes(attribute.Bool("shadow.actions_match", match))
	if !match {
		c.logger.Warn("shadow actions mismatch",
			slog.String("shadow.model", c.model))
	}
}

// fail records a failed comparison on the failure counter and span.
func (c *Comparator) fail(ctx context.Context, span trace.Span, reason string, err error) {
	c.failures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	span.SetAttributes(attribute.String("shadow.failure_reason", reason))
	span.SetStatus(codes.Error, reason)
	if err != nil {
		span.RecordError(err)
	}
	c.logger.Warn("shadow comparison failed",
		slog.String("reason", reason),
		slog.String("shadow.model", c.model),
		slog.Any("error", err))
}

// buildRequest returns an httpx.RequestFunc that POSTs the (model-rewritten)
// body to the shadow endpoint with the caller's headers. The body is rebuilt
// per attempt so retries always have a readable reader.
func (c *Comparator) buildRequest(in Input) httpx.RequestFunc {
	body := c.rewriteModel(in.Body)
	header := cloneHeader(in.Header)
	return func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = header
		return req, nil
	}
}

// rewriteModel returns body with its top-level "model" field replaced by the
// configured shadow model. If no model is configured or body is not a JSON
// object, the body is returned unchanged.
func (c *Comparator) rewriteModel(body []byte) []byte {
	if c.model == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	model, err := json.Marshal(c.model)
	if err != nil {
		return body
	}
	obj["model"] = model
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

// extractActions parses payload as a JSON object and returns its "action"
// field decoded into a generic value for comparison. ok is false when payload
// is not a JSON object (i.e. not parsable in the expected shape). A missing
// "action" key parses fine and yields a nil value.
func extractActions(payload []byte) (actions any, ok bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, false
	}
	raw, present := obj["action"]
	if !present {
		return nil, true
	}
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, false
	}
	return actions, true
}

// cloneHeader clones h and drops headers that no longer apply once the body has
// been rebuilt for the shadow request.
func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	clone := h.Clone()
	clone.Del("Content-Length")
	return clone
}

// isTimeout reports whether err represents a deadline/timeout (context deadline
// or a net.Error timeout, including the http.Client timeout).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
