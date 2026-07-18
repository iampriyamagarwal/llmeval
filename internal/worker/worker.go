// Package worker provides a simple in-process pool for running background jobs
// off the request path. Jobs are best-effort: they are not persisted, so any
// still queued or in flight when the process exits (crash or redeploy) are
// lost. Use it for fire-and-forget work where that trade-off is acceptable.
package worker

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
)

// tracerName is the instrumentation scope for job spans.
const tracerName = "llmeval/internal/worker"

// Job is the unit of work executed by the pool. It receives a fresh background
// context (not the request's), so it may outlive the request that enqueued it.
type Job func(ctx context.Context) error

// Pool runs Jobs on a fixed set of worker goroutines fed by a buffered queue.
type Pool struct {
	jobs   chan Job
	wg     sync.WaitGroup
	logger *slog.Logger
}

// New creates a Pool and starts workers goroutines reading from a queue of the
// given size. workers and queueSize are clamped to a minimum of 1.
func New(logger *slog.Logger, workers, queueSize int) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	if workers < 1 {
		workers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}

	p := &Pool{
		jobs:   make(chan Job, queueSize),
		logger: logger,
	}

	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.run()
	}

	return p
}

// run is the worker loop. It exits when the jobs channel is closed and drained.
func (p *Pool) run() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.execute(job)
	}
}

// execute runs a single job, recovering from panics so a misbehaving job can
// never take down a worker goroutine. Each job gets its own trace span.
func (p *Pool) execute(job Job) {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "worker.job")
	defer span.End()

	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("worker job panicked", slog.Any("panic", r))
		}
	}()

	if err := job(ctx); err != nil {
		p.logger.Error("worker job failed", slog.Any("error", err))
	}
}

// Submit enqueues a job without blocking. It returns false immediately if the
// queue is full, letting the caller shed load (e.g. respond 503) rather than
// stall the request path.
func (p *Pool) Submit(job Job) bool {
	select {
	case p.jobs <- job:
		return true
	default:
		return false
	}
}

// Shutdown stops accepting new jobs and waits for in-flight and queued jobs to
// finish. If ctx is cancelled or times out first, it returns ctx.Err() and any
// remaining work is abandoned when the process exits.
func (p *Pool) Shutdown(ctx context.Context) error {
	close(p.jobs)

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
