package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wenxichang/herdr-pal/internal/processlock"
)

const defaultLifecycleShutdownTimeout = 2*defaultWorkerShutdownTimeout + time.Second

var (
	// ErrInvalidSupervisor 表示顶层 Pal Supervisor 依赖或运行路径无效。
	ErrInvalidSupervisor = errors.New("Pal Supervisor 无效")
	// ErrLifecycleComponentStopped 表示常驻生命周期组件在未取消时意外返回。
	ErrLifecycleComponentStopped = errors.New("Pal 生命周期组件意外停止")
	// ErrLifecycleShutdownTimeout 表示 Supervisor 的子组件未能及时结束。
	ErrLifecycleShutdownTimeout = errors.New("Pal Supervisor 清理超时")
)

// LifecycleLock 是 Supervisor 整个生命周期内持有的实例锁。
type LifecycleLock interface {
	Release() error
}

// LockAcquireFunc 获取一个非阻塞本地实例锁。
type LockAcquireFunc func(string) (LifecycleLock, error)

// Runner 是可以随 context 取消的生命周期组件。
type Runner interface {
	Run(context.Context) error
}

// ReadyRunner 是在第一次确认 Herdr 存活后发出就绪信号的组件。
type ReadyRunner interface {
	Runner
	Ready() <-chan struct{}
}

// SupervisorOptions 装配顶层 Pal Supervisor 的进程锁和三个常驻组件。
type SupervisorOptions struct {
	Paths           RuntimePaths
	Status          *StatusStore
	Monitor         ReadyRunner
	Worker          Runner
	Control         StatusServer
	AcquireLock     LockAcquireFunc
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

// Supervisor 持有单实例锁并协调控制端点、Herdr Monitor 与 WorkerSupervisor。
type Supervisor struct {
	paths   RuntimePaths
	status  *StatusStore
	monitor ReadyRunner
	worker  Runner
	control StatusServer

	acquireLock     LockAcquireFunc
	shutdownTimeout time.Duration
	logger          *slog.Logger
}

// NewSupervisor 创建顶层 Pal Supervisor。
func NewSupervisor(options SupervisorOptions) (*Supervisor, error) {
	if options.Paths.OwnerLock == "" || options.Paths.ControlSocket == "" || options.Status == nil ||
		options.Monitor == nil || options.Worker == nil || options.Control == nil {
		return nil, ErrInvalidSupervisor
	}
	if options.AcquireLock == nil {
		options.AcquireLock = func(path string) (LifecycleLock, error) { return processlock.Acquire(path) }
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = defaultLifecycleShutdownTimeout
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{
		paths: options.Paths, status: options.Status,
		monitor: options.Monitor, worker: options.Worker, control: options.Control,
		acquireLock: options.AcquireLock, shutdownTimeout: options.ShutdownTimeout,
		logger: options.Logger,
	}, nil
}

// Run 维护整个 Pal 托管生命周期，直到 Herdr 停止或调用方取消。
func (supervisor *Supervisor) Run(ctx context.Context) (runErr error) {
	if supervisor == nil || ctx == nil {
		return ErrInvalidSupervisor
	}
	lock, err := supervisor.acquireLock(supervisor.paths.OwnerLock)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil {
			runErr = errors.Join(runErr, releaseErr)
		}
	}()

	runContext, cancel := context.WithCancel(ctx)
	results := make(chan lifecycleComponentResult, 3)
	started := 0
	start := func(name string, runner func(context.Context) error) {
		started++
		go func() { results <- lifecycleComponentResult{name: name, err: runner(runContext)} }()
	}
	start("control", func(componentContext context.Context) error {
		return supervisor.control.Run(componentContext, supervisor.paths.ControlSocket, supervisor.status.Load)
	})
	start("herdr_monitor", supervisor.monitor.Run)

	var first lifecycleComponentResult
	workerStarted := false
	select {
	case <-ctx.Done():
		first = lifecycleComponentResult{name: "parent", err: ctx.Err()}
	case first = <-results:
	case <-supervisor.monitor.Ready():
		start("worker", supervisor.worker.Run)
		workerStarted = true
	}
	if workerStarted {
		select {
		case <-ctx.Done():
			first = lifecycleComponentResult{name: "parent", err: ctx.Err()}
		case first = <-results:
		}
	}

	supervisor.status.Update(func(status *Status) { status.State = StateStopping })
	cancel()
	remaining := started
	if first.name != "parent" {
		remaining--
	}
	cleanupErr := waitLifecycleComponents(results, remaining, supervisor.shutdownTimeout)
	runErr = lifecycleRootError(ctx, first)
	if cleanupErr != nil {
		runErr = errors.Join(runErr, cleanupErr)
	}
	return runErr
}

type lifecycleComponentResult struct {
	name string
	err  error
}

func lifecycleRootError(parent context.Context, result lifecycleComponentResult) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if result.name == "herdr_monitor" && errors.Is(result.err, ErrHerdrStopped) {
		return nil
	}
	if result.err == nil || errors.Is(result.err, context.Canceled) {
		return ErrLifecycleComponentStopped
	}
	return result.err
}

func waitLifecycleComponents(results <-chan lifecycleComponentResult, count int, timeout time.Duration) error {
	if count <= 0 {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for count > 0 {
		select {
		case <-results:
			count--
		case <-timer.C:
			return ErrLifecycleShutdownTimeout
		}
	}
	return nil
}
