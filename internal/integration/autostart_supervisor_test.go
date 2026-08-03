//go:build !windows

package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/lifecycle"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

func TestAutostartSupervisorTracksHerdrLifecycle(t *testing.T) {
	herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("autostart-session", herdr.AgentStatusIdle))
	client := herdr.NewClient(herdrServer.SocketPath(), nil, time.Second)
	probe, err := lifecycle.NewPublicProbe(client)
	if err != nil {
		t.Fatal(err)
	}
	status := lifecycle.NewStatusStore(lifecycle.Status{State: lifecycle.StateStarting, Herdr: lifecycle.HerdrUnknown})
	monitor, err := lifecycle.NewHerdrMonitor(probe, status, lifecycle.MonitorOptions{
		NormalInterval: 20 * time.Millisecond,
		RetryInterval:  10 * time.Millisecond,
		ProbeTimeout:   100 * time.Millisecond,
		GracePeriod:    180 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeDirectory, err := os.MkdirTemp("", "herdr-pal-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDirectory) })
	paths := lifecycle.RuntimePaths{
		OwnerLock:     filepath.Join(runtimeDirectory, "owner.lock"),
		ControlSocket: filepath.Join(runtimeDirectory, "control", "status.sock"),
	}
	worker := newIntegrationLifecycleWorker(status)
	supervisor, err := lifecycle.NewSupervisor(lifecycle.SupervisorOptions{
		Paths:           paths,
		Status:          status,
		Monitor:         monitor,
		Worker:          worker,
		Control:         lifecycle.NewControlServer(),
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() { result <- supervisor.Run(context.Background()) }()
	worker.WaitStarted(t)
	herdrServer.WaitCallCount(t, "ping", 1)
	herdrServer.WaitCallCount(t, "session.snapshot", 1)
	waitForLifecycleStatus(t, paths.ControlSocket, func(current lifecycle.Status) bool {
		return current.State == lifecycle.StateRunning && current.Herdr == lifecycle.HerdrHealthy
	})

	herdrServer.SetAvailable(false)
	waitForLifecycleStatus(t, paths.ControlSocket, func(current lifecycle.Status) bool {
		return current.State == lifecycle.StateHerdrGrace && current.Herdr == lifecycle.HerdrUnavailable
	})
	herdrServer.SetAvailable(true)
	waitForLifecycleStatus(t, paths.ControlSocket, func(current lifecycle.Status) bool {
		return current.State == lifecycle.StateRunning && current.Herdr == lifecycle.HerdrHealthy
	})
	if got := worker.StartCount(); got != 1 {
		t.Fatalf("短暂断连后 Worker 启动次数 = %d, want 1", got)
	}

	herdrServer.SetAvailable(false)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Herdr 持续断连后 Supervisor error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Herdr 持续断连后 Supervisor 未退出")
	}
	worker.WaitStopped(t)
	if _, err := os.Lstat(paths.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("Supervisor 退出后控制 Socket 仍存在：%v", err)
	}
	lock, err := processlock.Acquire(paths.OwnerLock)
	if err != nil {
		t.Fatalf("Supervisor 退出后实例锁未释放：%v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func waitForLifecycleStatus(t *testing.T, socketPath string, accept func(lifecycle.Status) bool) lifecycle.Status {
	t.Helper()
	client := lifecycle.NewControlClient()
	deadline := time.Now().Add(2 * time.Second)
	var last lifecycle.Status
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		current, err := client.Status(ctx, socketPath)
		cancel()
		if err == nil {
			last = current
			if accept(current) {
				return current
			}
		} else if !errors.Is(err, lifecycle.ErrControlUnavailable) {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 Pal 生命周期状态超时，last=%#v error=%v", last, lastErr)
	return lifecycle.Status{}
}

type integrationLifecycleWorker struct {
	status  *lifecycle.StatusStore
	started chan struct{}
	stopped chan struct{}

	mu         sync.Mutex
	startCount int
}

func newIntegrationLifecycleWorker(status *lifecycle.StatusStore) *integrationLifecycleWorker {
	return &integrationLifecycleWorker{status: status, started: make(chan struct{}), stopped: make(chan struct{})}
}

func (worker *integrationLifecycleWorker) Run(ctx context.Context) error {
	worker.mu.Lock()
	worker.startCount++
	worker.mu.Unlock()
	worker.status.Update(func(status *lifecycle.Status) {
		status.State = lifecycle.StateRunning
		status.WorkerPID = 100
	})
	close(worker.started)
	<-ctx.Done()
	worker.status.Update(func(status *lifecycle.Status) { status.WorkerPID = 0 })
	close(worker.stopped)
	return ctx.Err()
}

func (worker *integrationLifecycleWorker) WaitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-worker.started:
	case <-time.After(time.Second):
		t.Fatal("Pal Worker 未启动")
	}
}

func (worker *integrationLifecycleWorker) WaitStopped(t *testing.T) {
	t.Helper()
	select {
	case <-worker.stopped:
	case <-time.After(time.Second):
		t.Fatal("Pal Worker 未停止")
	}
}

func (worker *integrationLifecycleWorker) StartCount() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return worker.startCount
}
