package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// discardLogger returns a logger that drops all output, keeping tests quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewClampsWorkersAndQueue(t *testing.T) {
	p := New(discardLogger(), 0, 0)
	if cap(p.jobs) != 1 {
		t.Errorf("queue cap = %d, want 1 (clamped)", cap(p.jobs))
	}
	// A single clamped worker should still drain a submitted job.
	done := make(chan struct{})
	if !p.Submit(func(ctx context.Context) error { close(done); return nil }) {
		t.Fatal("Submit returned false on empty queue")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("clamped worker did not run the job")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSubmitRunsJobs(t *testing.T) {
	p := New(discardLogger(), 4, 16)

	var (
		wg  sync.WaitGroup
		ran int32
	)
	const jobs = 8
	wg.Add(jobs)
	for i := 0; i < jobs; i++ {
		if !p.Submit(func(ctx context.Context) error {
			atomic.AddInt32(&ran, 1)
			wg.Done()
			return nil
		}) {
			t.Fatalf("Submit returned false for job %d", i)
		}
	}
	wg.Wait()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := atomic.LoadInt32(&ran); got != jobs {
		t.Errorf("ran = %d, want %d", got, jobs)
	}
}

func TestSubmitReturnsFalseWhenQueueFull(t *testing.T) {
	// One worker, queue of one. Block the worker so the queue backs up.
	p := New(discardLogger(), 1, 1)

	release := make(chan struct{})
	started := make(chan struct{})
	// Occupy the sole worker.
	if !p.Submit(func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	}) {
		t.Fatal("first Submit returned false")
	}
	<-started // worker is now busy

	// Fill the single queue slot.
	if !p.Submit(func(ctx context.Context) error { return nil }) {
		t.Fatal("second Submit returned false; queue slot should be free")
	}
	// Queue is full and worker is busy: next Submit must shed load.
	if p.Submit(func(ctx context.Context) error { return nil }) {
		t.Fatal("Submit returned true with a full queue; expected load shedding")
	}

	close(release)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestExecuteRecoversFromPanic(t *testing.T) {
	// One worker with room for both jobs so the follow-up is guaranteed to
	// enqueue behind the panicking one and run once the worker recovers.
	p := New(discardLogger(), 1, 2)

	done := make(chan struct{})
	if !p.Submit(func(ctx context.Context) error { panic("boom") }) {
		t.Fatal("Submit returned false")
	}
	// A follow-up job proves the worker survived the panic.
	if !p.Submit(func(ctx context.Context) error { close(done); return nil }) {
		t.Fatal("Submit returned false for follow-up job")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not survive panicking job")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestShutdownWaitsForQueuedJobs(t *testing.T) {
	p := New(discardLogger(), 2, 8)

	var ran int32
	const jobs = 6
	for i := 0; i < jobs; i++ {
		p.Submit(func(ctx context.Context) error {
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&ran, 1)
			return nil
		})
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := atomic.LoadInt32(&ran); got != jobs {
		t.Errorf("ran = %d, want %d (Shutdown should drain queued work)", got, jobs)
	}
}

func TestShutdownReturnsContextErrorOnTimeout(t *testing.T) {
	p := New(discardLogger(), 1, 1)

	release := make(chan struct{})
	started := make(chan struct{})
	p.Submit(func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context.DeadlineExceeded", err)
	}

	// Let the abandoned job finish so the goroutine can exit cleanly.
	close(release)
}
