package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

const (
	defaultWorkerStableWindow    = 30 * time.Second
	defaultWorkerShutdownTimeout = 10 * time.Second
	defaultWorkerHealthPoll      = 100 * time.Millisecond
)

var (
	// ErrInvalidWorkerSupervisor 表示 Worker 监督器依赖或参数无效。
	ErrInvalidWorkerSupervisor = errors.New("Pal Worker 监督器无效")
	// ErrWorkerShutdownTimeout 表示 Worker 在终止和强制结束后仍未及时返回。
	ErrWorkerShutdownTimeout = errors.New("Pal Worker 退出超时")
)

// WorkerProcess 表示由 Supervisor 创建并独占管理的一个 Pal Worker 子进程。
type WorkerProcess interface {
	PID() int
	Wait() error
	Terminate() error
	Kill() error
}

// WorkerFactory 创建一个新的 Pal Worker 子进程。
type WorkerFactory interface {
	Start() (WorkerProcess, error)
}

// RetryPolicy 提供 Worker 重启所需的有上限退避。
type RetryPolicy interface {
	Next() time.Duration
	Reset()
}

// WorkerWaitFunc 提供可取消等待，允许单元测试使用虚拟时间。
type WorkerWaitFunc func(context.Context, time.Duration) error

// WorkerSupervisorOptions 配置 Worker 重启和退出行为。
type WorkerSupervisorOptions struct {
	Backoff         RetryPolicy
	Wait            WorkerWaitFunc
	Now             func() time.Time
	StableWindow    time.Duration
	ShutdownTimeout time.Duration
	HealthPoll      time.Duration
	Logger          *slog.Logger
}

// WorkerSupervisor 在 Herdr 存活期间启动并恢复 Pal 业务 Worker。
type WorkerSupervisor struct {
	factory WorkerFactory
	status  *StatusStore

	backoff         RetryPolicy
	wait            WorkerWaitFunc
	now             func() time.Time
	stableWindow    time.Duration
	shutdownTimeout time.Duration
	healthPoll      time.Duration
	logger          *slog.Logger
}

// NewWorkerSupervisor 创建 Worker 子进程监督器。
func NewWorkerSupervisor(factory WorkerFactory, status *StatusStore, options WorkerSupervisorOptions) (*WorkerSupervisor, error) {
	if factory == nil || status == nil || options.Backoff == nil {
		return nil, ErrInvalidWorkerSupervisor
	}
	if options.Wait == nil {
		options.Wait = waitWorkerDelay
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.StableWindow <= 0 {
		options.StableWindow = defaultWorkerStableWindow
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = defaultWorkerShutdownTimeout
	}
	if options.HealthPoll <= 0 {
		options.HealthPoll = defaultWorkerHealthPoll
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &WorkerSupervisor{
		factory: factory, status: status,
		backoff: options.Backoff, wait: options.Wait, now: options.Now,
		stableWindow: options.StableWindow, shutdownTimeout: options.ShutdownTimeout,
		healthPoll: options.HealthPoll, logger: options.Logger,
	}, nil
}

// Run 维护一个 Worker，直到 context 取消。
func (supervisor *WorkerSupervisor) Run(ctx context.Context) error {
	if supervisor == nil || ctx == nil {
		return ErrInvalidWorkerSupervisor
	}
	for {
		if err := supervisor.waitForHerdr(ctx); err != nil {
			return err
		}
		startedAt := supervisor.now()
		process, err := supervisor.factory.Start()
		if err != nil {
			supervisor.markWorkerBackoff()
			supervisor.logger.Error("Pal Worker 启动失败", "error_type", "start")
			if err := supervisor.waitForRestart(ctx); err != nil {
				return err
			}
			continue
		}
		supervisor.markWorkerRunning(process.PID())
		supervisor.logger.Info("Pal Worker 已启动", "worker_pid", process.PID())
		waitResult := make(chan error, 1)
		go func() { waitResult <- process.Wait() }()

		select {
		case waitErr := <-waitResult:
			runDuration := supervisor.now().Sub(startedAt)
			supervisor.markWorkerExited()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if runDuration >= supervisor.stableWindow {
				supervisor.backoff.Reset()
			}
			supervisor.logger.Warn("Pal Worker 已退出", "worker_pid", process.PID(), "run_duration", runDuration, "error_type", workerExitErrorType(waitErr))
			if err := supervisor.waitForRestart(ctx); err != nil {
				return err
			}
		case <-ctx.Done():
			supervisor.markStopping()
			shutdownErr := supervisor.stopWorker(process, waitResult)
			if shutdownErr != nil {
				return errors.Join(ctx.Err(), shutdownErr)
			}
			return ctx.Err()
		}
	}
}

func (supervisor *WorkerSupervisor) waitForRestart(ctx context.Context) error {
	if err := supervisor.waitForHerdr(ctx); err != nil {
		return err
	}
	delay := supervisor.backoff.Next()
	supervisor.markWorkerBackoff()
	supervisor.logger.Info("Pal Worker 等待重启", "retry_delay", delay)
	return supervisor.wait(ctx, delay)
}

func (supervisor *WorkerSupervisor) waitForHerdr(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		herdrState := supervisor.status.Load().Herdr
		if herdrState == HerdrHealthy || herdrState == HerdrIncompatible {
			return nil
		}
		if err := supervisor.wait(ctx, supervisor.healthPoll); err != nil {
			return err
		}
	}
}

func (supervisor *WorkerSupervisor) stopWorker(process WorkerProcess, waitResult <-chan error) error {
	terminateErr := process.Terminate()
	timer := time.NewTimer(supervisor.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-waitResult:
		return terminateErr
	case <-timer.C:
	}
	killErr := process.Kill()
	secondTimer := time.NewTimer(supervisor.shutdownTimeout)
	defer secondTimer.Stop()
	select {
	case <-waitResult:
		return errors.Join(terminateErr, killErr)
	case <-secondTimer.C:
		return errors.Join(terminateErr, killErr, ErrWorkerShutdownTimeout)
	}
}

func (supervisor *WorkerSupervisor) markWorkerRunning(pid int) {
	supervisor.status.Update(func(status *Status) {
		status.WorkerPID = pid
		if status.Herdr == HerdrUnavailable {
			status.State = StateHerdrGrace
		} else {
			status.State = StateRunning
		}
	})
}

func (supervisor *WorkerSupervisor) markWorkerExited() {
	supervisor.status.Update(func(status *Status) {
		status.WorkerPID = 0
		if status.Herdr == HerdrUnavailable {
			status.State = StateHerdrGrace
		} else {
			status.State = StateWorkerBackoff
		}
	})
}

func (supervisor *WorkerSupervisor) markWorkerBackoff() {
	supervisor.status.Update(func(status *Status) {
		status.WorkerPID = 0
		if status.Herdr != HerdrUnavailable {
			status.State = StateWorkerBackoff
		}
	})
}

func (supervisor *WorkerSupervisor) markStopping() {
	supervisor.status.Update(func(status *Status) {
		status.State = StateStopping
	})
}

func waitWorkerDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func workerExitErrorType(err error) string {
	if err == nil {
		return "unexpected_clean_exit"
	}
	return "process_exit"
}

// CommandWorkerOptions 描述生产 Worker 子进程的可执行文件、配置和输出目标。
type CommandWorkerOptions struct {
	Executable  string
	ConfigPath  string
	SocketPath  string
	Environment []string
	Stdout      io.Writer
	Stderr      io.Writer
}
