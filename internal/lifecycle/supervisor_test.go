package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSupervisorStopsNormallyWhenHerdrStops(t *testing.T) {
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrUnknown})
	monitor := newFakeReadyRunner()
	worker := newFakeRunner()
	control := newFakeControlServer()
	lock := &fakeLifecycleLock{}
	supervisor, err := NewSupervisor(SupervisorOptions{
		Paths:  RuntimePaths{OwnerLock: "/locks/owner.lock", ControlSocket: "/control/status.sock"},
		Status: store, Monitor: monitor, Worker: worker, Control: control,
		AcquireLock: func(path string) (LifecycleLock, error) {
			if path != "/locks/owner.lock" {
				t.Fatalf("lock path = %q", path)
			}
			return lock, nil
		},
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(context.Background()) }()
	monitor.MarkReady()
	worker.WaitStarted(t)
	control.WaitStarted(t)
	monitor.Finish(ErrHerdrStopped)
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !lock.Released() {
		t.Fatal("owner lock was not released")
	}
	if got := store.Load(); got.State != StateStopping {
		t.Fatalf("final status = %#v", got)
	}
}

func TestDefaultLifecycleShutdownTimeoutCoversWorkerStop(t *testing.T) {
	if defaultLifecycleShutdownTimeout <= 2*defaultWorkerShutdownTimeout {
		t.Fatalf("lifecycle shutdown timeout %s does not cover worker stop budget %s", defaultLifecycleShutdownTimeout, 2*defaultWorkerShutdownTimeout)
	}
}

func TestSupervisorReturnsUnexpectedComponentFailure(t *testing.T) {
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrUnknown})
	monitor := newFakeReadyRunner()
	worker := newFakeRunner()
	control := newFakeControlServer()
	controlErr := errors.New("control failed")
	control.finish = controlErr
	lock := &fakeLifecycleLock{}
	supervisor, err := NewSupervisor(SupervisorOptions{
		Paths:  RuntimePaths{OwnerLock: "/locks/owner.lock", ControlSocket: "/control/status.sock"},
		Status: store, Monitor: monitor, Worker: worker, Control: control,
		AcquireLock:     func(string) (LifecycleLock, error) { return lock, nil },
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	monitor.MarkReady()
	err = supervisor.Run(context.Background())
	if !errors.Is(err, controlErr) {
		t.Fatalf("Run() error = %v", err)
	}
	if !lock.Released() {
		t.Fatal("owner lock was not released")
	}
}

type fakeReadyRunner struct {
	ready      chan struct{}
	finish     chan error
	readyOnce  sync.Once
	finishOnce sync.Once
}

func newFakeReadyRunner() *fakeReadyRunner {
	return &fakeReadyRunner{ready: make(chan struct{}), finish: make(chan error, 1)}
}

func (runner *fakeReadyRunner) Ready() <-chan struct{} { return runner.ready }

func (runner *fakeReadyRunner) MarkReady() { runner.readyOnce.Do(func() { close(runner.ready) }) }

func (runner *fakeReadyRunner) Finish(err error) {
	runner.finishOnce.Do(func() { runner.finish <- err })
}

func (runner *fakeReadyRunner) Run(ctx context.Context) error {
	select {
	case err := <-runner.finish:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fakeRunner struct {
	started chan struct{}
}

func newFakeRunner() *fakeRunner { return &fakeRunner{started: make(chan struct{}, 1)} }

func (runner *fakeRunner) Run(ctx context.Context) error {
	runner.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func (runner *fakeRunner) WaitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
}

type fakeControlServer struct {
	started chan struct{}
	finish  error
}

func newFakeControlServer() *fakeControlServer {
	return &fakeControlServer{started: make(chan struct{}, 1)}
}

func (server *fakeControlServer) Run(ctx context.Context, _ string, current func() Status) error {
	_ = current()
	server.started <- struct{}{}
	if server.finish != nil {
		return server.finish
	}
	<-ctx.Done()
	return nil
}

func (server *fakeControlServer) WaitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("control server did not start")
	}
}

type fakeLifecycleLock struct {
	mu       sync.Mutex
	released bool
}

func (lock *fakeLifecycleLock) Release() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	lock.released = true
	return nil
}

func (lock *fakeLifecycleLock) Released() bool {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.released
}
