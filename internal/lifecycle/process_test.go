package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWorkerSupervisorRestartsWithBackoff(t *testing.T) {
	clock := newWorkerClock()
	processes := []*fakeWorkerProcess{
		newExitedWorker(errors.New("first crash")),
		newExitedWorker(errors.New("second crash")),
		newBlockingWorker(true),
	}
	factory := newFakeWorkerFactory(processes...)
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrHealthy})
	var delays []time.Duration
	supervisor := newTestWorkerSupervisor(t, factory, store, clock, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		clock.Advance(delay)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	factory.WaitForStarts(t, 3)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(delays) != 2 || delays[0] != time.Second || delays[1] != 2*time.Second {
		t.Fatalf("backoff delays = %v", delays)
	}
	if processes[2].TerminateCalls() != 1 || processes[2].KillCalls() != 0 {
		t.Fatalf("shutdown calls terminate=%d kill=%d", processes[2].TerminateCalls(), processes[2].KillCalls())
	}
}

func TestWorkerSupervisorResetsBackoffAfterStableWindow(t *testing.T) {
	clock := newWorkerClock()
	stable := newExitedWorker(errors.New("stable exit"))
	stable.onWait = func() { clock.Advance(31 * time.Second) }
	processes := []*fakeWorkerProcess{
		newExitedWorker(errors.New("first crash")),
		newExitedWorker(errors.New("second crash")),
		stable,
		newBlockingWorker(true),
	}
	factory := newFakeWorkerFactory(processes...)
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrHealthy})
	var delays []time.Duration
	supervisor := newTestWorkerSupervisor(t, factory, store, clock, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		clock.Advance(delay)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	factory.WaitForStarts(t, 4)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if len(delays) != len(want) {
		t.Fatalf("backoff delays = %v", delays)
	}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("backoff delays = %v, want %v", delays, want)
		}
	}
}

func TestWorkerSupervisorDoesNotRestartDuringHerdrGrace(t *testing.T) {
	clock := newWorkerClock()
	store := NewStatusStore(Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 1})
	first := newExitedWorker(errors.New("worker exited"))
	first.onWait = func() {
		store.Update(func(status *Status) {
			status.State = StateHerdrGrace
			status.Herdr = HerdrUnavailable
		})
	}
	factory := newFakeWorkerFactory(first, newBlockingWorker(true))
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	supervisor := newTestWorkerSupervisor(t, factory, store, clock, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		cancel()
		return nil
	})
	err := supervisor.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if factory.Starts() != 1 {
		t.Fatalf("worker starts = %d, want 1", factory.Starts())
	}
	if len(delays) != 1 || delays[0] != 100*time.Millisecond {
		t.Fatalf("grace waits = %v", delays)
	}
}

func TestWorkerSupervisorKillsWorkerAfterShutdownTimeout(t *testing.T) {
	process := newBlockingWorker(false)
	factory := newFakeWorkerFactory(process)
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrHealthy})
	backoff, err := NewBackoff(time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("NewBackoff() error = %v", err)
	}
	supervisor, err := NewWorkerSupervisor(factory, store, WorkerSupervisorOptions{
		Backoff:         backoff,
		StableWindow:    30 * time.Second,
		ShutdownTimeout: 20 * time.Millisecond,
		HealthPoll:      100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWorkerSupervisor() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	factory.WaitForStarts(t, 1)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if process.TerminateCalls() != 1 || process.KillCalls() != 1 {
		t.Fatalf("shutdown calls terminate=%d kill=%d", process.TerminateCalls(), process.KillCalls())
	}
}

func newTestWorkerSupervisor(
	t *testing.T,
	factory WorkerFactory,
	store *StatusStore,
	clock *workerClock,
	wait WorkerWaitFunc,
) *WorkerSupervisor {
	t.Helper()
	backoff, err := NewBackoff(time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("NewBackoff() error = %v", err)
	}
	supervisor, err := NewWorkerSupervisor(factory, store, WorkerSupervisorOptions{
		Backoff:         backoff,
		Wait:            wait,
		Now:             clock.Now,
		StableWindow:    30 * time.Second,
		ShutdownTimeout: time.Second,
		HealthPoll:      100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWorkerSupervisor() error = %v", err)
	}
	return supervisor
}

type fakeWorkerFactory struct {
	mu        sync.Mutex
	processes []*fakeWorkerProcess
	starts    int
	started   chan struct{}
}

func newFakeWorkerFactory(processes ...*fakeWorkerProcess) *fakeWorkerFactory {
	return &fakeWorkerFactory{processes: processes, started: make(chan struct{}, len(processes)+1)}
}

func (factory *fakeWorkerFactory) Start() (WorkerProcess, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.starts >= len(factory.processes) {
		return nil, errors.New("unexpected worker start")
	}
	process := factory.processes[factory.starts]
	factory.starts++
	factory.started <- struct{}{}
	return process, nil
}

func (factory *fakeWorkerFactory) Starts() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.starts
}

func (factory *fakeWorkerFactory) WaitForStarts(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for factory.Starts() < count {
		select {
		case <-factory.started:
		case <-deadline:
			t.Fatalf("worker starts = %d, want %d", factory.Starts(), count)
		}
	}
}

type fakeWorkerProcess struct {
	mu              sync.Mutex
	waitResult      chan error
	onWait          func()
	exitOnTerminate bool
	terminateCalls  int
	killCalls       int
	finishOnce      sync.Once
}

func newExitedWorker(err error) *fakeWorkerProcess {
	result := make(chan error, 1)
	result <- err
	return &fakeWorkerProcess{waitResult: result}
}

func newBlockingWorker(exitOnTerminate bool) *fakeWorkerProcess {
	return &fakeWorkerProcess{waitResult: make(chan error, 1), exitOnTerminate: exitOnTerminate}
}

func (process *fakeWorkerProcess) PID() int { return 100 }

func (process *fakeWorkerProcess) Wait() error {
	if process.onWait != nil {
		process.onWait()
	}
	return <-process.waitResult
}

func (process *fakeWorkerProcess) Terminate() error {
	process.mu.Lock()
	process.terminateCalls++
	exit := process.exitOnTerminate
	process.mu.Unlock()
	if exit {
		process.finish(nil)
	}
	return nil
}

func (process *fakeWorkerProcess) Kill() error {
	process.mu.Lock()
	process.killCalls++
	process.mu.Unlock()
	process.finish(errors.New("killed"))
	return nil
}

func (process *fakeWorkerProcess) finish(err error) {
	process.finishOnce.Do(func() { process.waitResult <- err })
}

func (process *fakeWorkerProcess) TerminateCalls() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.terminateCalls
}

func (process *fakeWorkerProcess) KillCalls() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.killCalls
}

type workerClock struct {
	mu  sync.Mutex
	now time.Time
}

func newWorkerClock() *workerClock { return &workerClock{now: time.Unix(0, 0)} }

func (clock *workerClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *workerClock) Advance(delay time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delay)
}
