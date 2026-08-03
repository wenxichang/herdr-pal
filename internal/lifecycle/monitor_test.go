package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestHerdrMonitorRecoversWithinGracePeriod(t *testing.T) {
	clock := newMonitorClock()
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	probe := &scriptedProbe{steps: []probeStep{
		{result: ProbeResult{Alive: true, Compatible: true}},
		{err: errors.New("temporary disconnect")},
		{err: errors.New("temporary disconnect")},
		{result: ProbeResult{Alive: true, Compatible: true}},
	}}
	store := NewStatusStore(Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 9})
	monitor := newTestHerdrMonitor(t, probe, store, clock, func(ctx context.Context, delay time.Duration) error {
		clock.Advance(delay)
		waits++
		if waits == 4 {
			cancel()
		}
		return nil
	})

	err := monitor.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	got := store.Load()
	if got.State != StateRunning || got.Herdr != HerdrHealthy || got.WorkerPID != 9 {
		t.Fatalf("status after recovery = %#v", got)
	}
	if probe.verifyCalls != 1 {
		t.Fatalf("VerifyReady() calls = %d", probe.verifyCalls)
	}
	select {
	case <-monitor.Ready():
	default:
		t.Fatal("Ready() was not closed")
	}
}

func TestHerdrMonitorRecoversToWorkerBackoffWhenWorkerExitedDuringGrace(t *testing.T) {
	clock := newMonitorClock()
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	probe := &scriptedProbe{steps: []probeStep{
		{result: ProbeResult{Alive: true, Compatible: true}},
		{err: errors.New("temporary disconnect")},
		{result: ProbeResult{Alive: true, Compatible: true}},
	}}
	store := NewStatusStore(Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 9})
	monitor := newTestHerdrMonitor(t, probe, store, clock, func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		waits++
		if waits == 2 {
			store.Update(func(status *Status) { status.WorkerPID = 0 })
		}
		if waits == 3 {
			cancel()
		}
		return nil
	})

	err := monitor.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	got := store.Load()
	if got.State != StateWorkerBackoff || got.Herdr != HerdrHealthy || got.WorkerPID != 0 {
		t.Fatalf("status after worker exit during grace = %#v", got)
	}
}

func TestHerdrMonitorStopsAfterSustainedFailure(t *testing.T) {
	clock := newMonitorClock()
	probe := &scriptedProbe{steps: []probeStep{
		{result: ProbeResult{Alive: true, Compatible: true}},
		{err: errors.New("server stopped")},
	}}
	store := NewStatusStore(Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 9})
	monitor := newTestHerdrMonitor(t, probe, store, clock, func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		return nil
	})

	err := monitor.Run(context.Background())
	if !errors.Is(err, ErrHerdrStopped) {
		t.Fatalf("Run() error = %v", err)
	}
	got := store.Load()
	if got.State != StateHerdrGrace || got.Herdr != HerdrUnavailable {
		t.Fatalf("status at stop = %#v", got)
	}
	if clock.Now().Sub(time.Unix(0, 0)) < 7*time.Second {
		t.Fatalf("monitor stopped before grace elapsed: %s", clock.Now())
	}
}

func TestHerdrMonitorKeepsIncompatibleServerAlive(t *testing.T) {
	clock := newMonitorClock()
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	probe := &scriptedProbe{steps: []probeStep{{result: ProbeResult{Alive: true, Compatible: false}}}}
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrUnknown})
	monitor := newTestHerdrMonitor(t, probe, store, clock, func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		waits++
		if waits == 2 {
			cancel()
		}
		return nil
	})

	err := monitor.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	got := store.Load()
	if got.Herdr != HerdrIncompatible || got.State != StateStarting {
		t.Fatalf("incompatible status = %#v", got)
	}
	if probe.verifyCalls != 0 {
		t.Fatalf("VerifyReady() calls = %d, want 0", probe.verifyCalls)
	}
	select {
	case <-monitor.Ready():
	default:
		t.Fatal("incompatible Herdr did not mark monitor ready")
	}
}

func newTestHerdrMonitor(t *testing.T, probe Probe, store *StatusStore, clock *monitorClock, wait MonitorWaitFunc) *HerdrMonitor {
	t.Helper()
	monitor, err := NewHerdrMonitor(probe, store, MonitorOptions{
		NormalInterval: 2 * time.Second,
		RetryInterval:  500 * time.Millisecond,
		ProbeTimeout:   time.Second,
		GracePeriod:    5 * time.Second,
		Wait:           wait,
		Now:            clock.Now,
	})
	if err != nil {
		t.Fatalf("NewHerdrMonitor() error = %v", err)
	}
	return monitor
}

type probeStep struct {
	result ProbeResult
	err    error
}

type scriptedProbe struct {
	mu          sync.Mutex
	steps       []probeStep
	index       int
	verifyErr   error
	verifyCalls int
}

func (probe *scriptedProbe) Probe(context.Context) (ProbeResult, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	index := probe.index
	if index >= len(probe.steps) {
		index = len(probe.steps) - 1
	} else {
		probe.index++
	}
	return probe.steps[index].result, probe.steps[index].err
}

func (probe *scriptedProbe) VerifyReady(context.Context) error {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.verifyCalls++
	return probe.verifyErr
}

type monitorClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMonitorClock() *monitorClock {
	return &monitorClock{now: time.Unix(0, 0)}
}

func (clock *monitorClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *monitorClock) Advance(delay time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delay)
}
