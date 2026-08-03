package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/processlock"
)

const (
	defaultLauncherStartTimeout = 5 * time.Second
	defaultLauncherPollInterval = 100 * time.Millisecond
)

var (
	// ErrInvalidLauncher 表示 Launcher 缺少运行路径、命令或依赖。
	ErrInvalidLauncher = errors.New("Pal Launcher 无效")
	// ErrLauncherBusy 表示另一个 Launcher 长时间占据启动串行锁。
	ErrLauncherBusy = errors.New("Pal Launcher 正在被另一个启动请求占用")
	// ErrSupervisorUnresponsive 表示实例锁存在但健康端点持续无响应。
	ErrSupervisorUnresponsive = errors.New("Pal Supervisor 已持锁但健康端点无响应")
	// ErrSupervisorStartFailed 表示已创建 Supervisor，但未能在启动窗口内进入可用状态。
	ErrSupervisorStartFailed = errors.New("Pal Supervisor 启动失败")
)

// SupervisorCommand 描述 Launcher 交给后台 Supervisor 的非敏感启动参数。
type SupervisorCommand struct {
	Executable string
	ConfigPath string
	SocketPath string
}

// SupervisorSpawner 创建一个脱离当前 startup 命令的后台 Supervisor。
type SupervisorSpawner interface {
	Spawn(SupervisorCommand) error
}

// LauncherWaitFunc 提供可取消轮询等待，允许测试使用虚拟时间。
type LauncherWaitFunc func(context.Context, time.Duration) error

// LauncherOptions 配置一次 startup 触发的幂等 Supervisor 启动过程。
type LauncherOptions struct {
	Paths      RuntimePaths
	ConfigPath string
	SocketPath string
	Executable string

	AcquireLock  LockAcquireFunc
	Control      StatusClient
	Spawner      SupervisorSpawner
	StartTimeout time.Duration
	PollInterval time.Duration
	Wait         LauncherWaitFunc
	Now          func() time.Time
	Logger       *slog.Logger
}

// Launcher 串行化重复 startup，复用健康 Supervisor 或创建新实例。
type Launcher struct {
	paths   RuntimePaths
	command SupervisorCommand

	acquireLock  LockAcquireFunc
	control      StatusClient
	spawner      SupervisorSpawner
	startTimeout time.Duration
	pollInterval time.Duration
	wait         LauncherWaitFunc
	now          func() time.Time
	logger       *slog.Logger
}

// NewLauncher 创建一个短生命周期 Pal 启动器。
func NewLauncher(options LauncherOptions) (*Launcher, error) {
	options.ConfigPath = strings.TrimSpace(options.ConfigPath)
	options.SocketPath = strings.TrimSpace(options.SocketPath)
	options.Executable = strings.TrimSpace(options.Executable)
	if options.Paths.StartLock == "" || options.Paths.OwnerLock == "" || options.Paths.ControlSocket == "" ||
		options.ConfigPath == "" || options.SocketPath == "" || options.Executable == "" || options.Spawner == nil {
		return nil, ErrInvalidLauncher
	}
	if options.AcquireLock == nil {
		options.AcquireLock = func(path string) (LifecycleLock, error) { return processlock.Acquire(path) }
	}
	if options.Control == nil {
		options.Control = NewControlClient()
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = defaultLauncherStartTimeout
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaultLauncherPollInterval
	}
	if options.Wait == nil {
		options.Wait = waitLauncherDelay
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &Launcher{
		paths:       options.Paths,
		command:     SupervisorCommand{Executable: options.Executable, ConfigPath: options.ConfigPath, SocketPath: options.SocketPath},
		acquireLock: options.AcquireLock, control: options.Control, spawner: options.Spawner,
		startTimeout: options.StartTimeout, pollInterval: options.PollInterval,
		wait: options.Wait, now: options.Now, logger: options.Logger,
	}, nil
}

// Start 在有限窗口内确认已有 Supervisor 或启动一个新实例。
func (launcher *Launcher) Start(ctx context.Context) error {
	if launcher == nil || ctx == nil {
		return ErrInvalidLauncher
	}
	deadline := launcher.now().Add(launcher.startTimeout)
	startLock, err := launcher.acquireStartLock(ctx, deadline)
	if err != nil {
		return err
	}
	defer startLock.Release()

	spawned := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ownerFree, status, statusErr, err := launcher.inspectOwner(ctx)
		if err != nil {
			return err
		}
		if ownerFree {
			if !spawned {
				launcher.logger.Info("启动 Pal Supervisor", "socket_hash", launcher.paths.InstanceHash)
				if err := launcher.spawner.Spawn(launcher.command); err != nil {
					return errors.Join(ErrSupervisorStartFailed, err)
				}
				spawned = true
			} else if !launcher.now().Before(deadline) {
				return ErrSupervisorStartFailed
			}
		} else if statusErr == nil && launcherStatusReady(status) {
			launcher.logger.Info("Pal Supervisor 已接管", "socket_hash", launcher.paths.InstanceHash, "state", status.State)
			return nil
		}

		if !launcher.now().Before(deadline) {
			if spawned {
				return ErrSupervisorStartFailed
			}
			return ErrSupervisorUnresponsive
		}
		if err := launcher.wait(ctx, launcher.pollInterval); err != nil {
			return err
		}
	}
}

func (launcher *Launcher) acquireStartLock(ctx context.Context, deadline time.Time) (LifecycleLock, error) {
	for {
		lock, err := launcher.acquireLock(launcher.paths.StartLock)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, processlock.ErrAlreadyRunning) {
			return nil, err
		}
		if !launcher.now().Before(deadline) {
			return nil, ErrLauncherBusy
		}
		if err := launcher.wait(ctx, launcher.pollInterval); err != nil {
			return nil, err
		}
	}
}

func (launcher *Launcher) inspectOwner(ctx context.Context) (bool, Status, error, error) {
	lock, err := launcher.acquireLock(launcher.paths.OwnerLock)
	if err == nil {
		if releaseErr := lock.Release(); releaseErr != nil {
			return false, Status{}, nil, releaseErr
		}
		return true, Status{}, nil, nil
	}
	if !errors.Is(err, processlock.ErrAlreadyRunning) {
		return false, Status{}, nil, err
	}
	status, statusErr := launcher.control.Status(ctx, launcher.paths.ControlSocket)
	return false, status, statusErr, nil
}

func launcherStatusReady(status Status) bool {
	return status.State == StateRunning || status.State == StateWorkerBackoff
}

func waitLauncherDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
