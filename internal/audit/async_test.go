package audit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type blockingBatchExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (exporter *blockingBatchExporter) Export(ctx context.Context, events []Event) error {
	exporter.once.Do(func() { close(exporter.started) })
	select {
	case <-exporter.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAsyncAuditorEmitIsNonBlockingAndDropsWhenQueueIsFull(t *testing.T) {
	exporter := &blockingBatchExporter{started: make(chan struct{}), release: make(chan struct{})}
	auditor, err := NewAsyncAuditor(exporter, slog.New(slog.NewTextHandler(io.Discard, nil)), AsyncConfig{
		MaxEvents: 1, MaxBytes: 1024, MaxBatchEvents: 1, MaxBatchBytes: 1024, FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewAsyncAuditor() error = %v", err)
	}
	auditor.Emit(Event{EventID: "1", Body: "first"})
	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("exporter did not start")
	}
	auditor.Emit(Event{EventID: "2", Body: "queued"})
	startedAt := time.Now()
	auditor.Emit(Event{EventID: "3", Body: "dropped"})
	if elapsed := time.Since(startedAt); elapsed > 50*time.Millisecond {
		t.Fatalf("Emit() blocked for %s", elapsed)
	}
	if stats := auditor.Stats(); stats.Enqueued != 2 || stats.Dropped != 1 {
		t.Fatalf("Stats() = %#v", stats)
	}
	close(exporter.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := auditor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

type collectingBatchExporter struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (exporter *collectingBatchExporter) Export(_ context.Context, events []Event) error {
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	exporter.events = append(exporter.events, events...)
	return exporter.err
}

func TestAsyncAuditorShutdownFlushesAcceptedEvents(t *testing.T) {
	exporter := &collectingBatchExporter{}
	auditor, err := NewAsyncAuditor(exporter, nil, AsyncConfig{FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	auditor.Emit(Event{EventID: "1", Body: "one"})
	auditor.Emit(Event{EventID: "2", Body: "two"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := auditor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.events) != 2 {
		t.Fatalf("events = %#v", exporter.events)
	}
}

func TestAsyncAuditorExportFailureDoesNotEscapeEmit(t *testing.T) {
	exporter := &collectingBatchExporter{err: errors.New("sensitive exporter failure")}
	auditor, err := NewAsyncAuditor(exporter, slog.New(slog.NewTextHandler(io.Discard, nil)), AsyncConfig{MaxBatchEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	auditor.Emit(Event{Body: "sensitive body"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := auditor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if stats := auditor.Stats(); stats.Failed != 1 {
		t.Fatalf("Stats() = %#v", stats)
	}
}
