package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultMonitorNormalInterval = 2 * time.Second
	defaultMonitorRetryInterval  = 500 * time.Millisecond
	defaultMonitorProbeTimeout   = time.Second
	defaultMonitorGracePeriod    = 5 * time.Second
)

var (
	// ErrHerdrStopped 表示 Herdr 公共 API 在整个退出宽限期内持续不可连接。
	ErrHerdrStopped = errors.New("Herdr Server 已停止")
	// ErrInvalidMonitor 表示 Herdr 生命周期监督器依赖或参数无效。
	ErrInvalidMonitor = errors.New("Herdr 生命周期监督器无效")
)

// MonitorWaitFunc 等待指定时长，并允许测试注入虚拟时钟。
type MonitorWaitFunc func(context.Context, time.Duration) error

// MonitorOptions 配置 Herdr 探测频率、退出宽限期和可测试依赖。
type MonitorOptions struct {
	NormalInterval time.Duration
	RetryInterval  time.Duration
	ProbeTimeout   time.Duration
	GracePeriod    time.Duration
	Wait           MonitorWaitFunc
	Now            func() time.Time
	Logger         *slog.Logger
}

// HerdrMonitor 使用公开 API 维护 Herdr 存活状态并确认真正退出。
type HerdrMonitor struct {
	probe  Probe
	status *StatusStore

	normalInterval time.Duration
	retryInterval  time.Duration
	probeTimeout   time.Duration
	gracePeriod    time.Duration
	wait           MonitorWaitFunc
	now            func() time.Time
	logger         *slog.Logger

	ready     chan struct{}
	readyOnce sync.Once
}

// NewHerdrMonitor 创建一个独立于业务 Worker 的 Herdr 生命周期监督器。
func NewHerdrMonitor(probe Probe, status *StatusStore, options MonitorOptions) (*HerdrMonitor, error) {
	if probe == nil || status == nil {
		return nil, ErrInvalidMonitor
	}
	if options.NormalInterval <= 0 {
		options.NormalInterval = defaultMonitorNormalInterval
	}
	if options.RetryInterval <= 0 {
		options.RetryInterval = defaultMonitorRetryInterval
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = defaultMonitorProbeTimeout
	}
	if options.GracePeriod <= 0 {
		options.GracePeriod = defaultMonitorGracePeriod
	}
	if options.Wait == nil {
		options.Wait = waitMonitorDelay
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &HerdrMonitor{
		probe: probe, status: status,
		normalInterval: options.NormalInterval,
		retryInterval:  options.RetryInterval,
		probeTimeout:   options.ProbeTimeout,
		gracePeriod:    options.GracePeriod,
		wait:           options.Wait, now: options.Now, logger: options.Logger,
		ready: make(chan struct{}),
	}, nil
}

// Ready 在第一次确认 Herdr 存活后关闭；协议不匹配也能证明 Server 存活。
func (monitor *HerdrMonitor) Ready() <-chan struct{} {
	if monitor == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return monitor.ready
}

// Run 持续探测 Herdr，直到 context 取消或确认 Server 已停止。
func (monitor *HerdrMonitor) Run(ctx context.Context) error {
	if monitor == nil || ctx == nil {
		return ErrInvalidMonitor
	}
	var graceStarted time.Time
	stateBeforeGrace := StateStarting
	readyVerified := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, probeErr := monitor.probeOnce(ctx)
		if result.Alive && result.Compatible && !readyVerified {
			if err := monitor.verifyReady(ctx); err != nil {
				result = ProbeResult{}
				probeErr = err
			} else {
				readyVerified = true
			}
		}

		now := monitor.now()
		delay := monitor.normalInterval
		if result.Alive {
			monitor.readyOnce.Do(func() { close(monitor.ready) })
			monitor.markAvailable(result.Compatible, stateBeforeGrace)
			if !graceStarted.IsZero() {
				monitor.logger.Info("Herdr 公共 API 已恢复", "outage_duration", now.Sub(graceStarted))
			}
			graceStarted = time.Time{}
		} else {
			if graceStarted.IsZero() {
				graceStarted = now
				stateBeforeGrace = monitor.status.Load().State
				monitor.logger.Warn("Herdr 公共 API 暂时不可连接", "grace_period", monitor.gracePeriod, "error_type", monitorProbeErrorType(probeErr))
			}
			monitor.markUnavailable()
			if !now.Before(graceStarted.Add(monitor.gracePeriod)) {
				monitor.logger.Info("Herdr 公共 API 持续不可连接，确认 Server 已停止", "outage_duration", now.Sub(graceStarted))
				return ErrHerdrStopped
			}
			delay = monitor.retryInterval
		}
		if err := monitor.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (monitor *HerdrMonitor) probeOnce(ctx context.Context) (ProbeResult, error) {
	probeContext, cancel := context.WithTimeout(ctx, monitor.probeTimeout)
	defer cancel()
	return monitor.probe.Probe(probeContext)
}

func (monitor *HerdrMonitor) verifyReady(ctx context.Context) error {
	probeContext, cancel := context.WithTimeout(ctx, monitor.probeTimeout)
	defer cancel()
	return monitor.probe.VerifyReady(probeContext)
}

func (monitor *HerdrMonitor) markAvailable(compatible bool, stateBeforeGrace State) {
	monitor.status.Update(func(status *Status) {
		if compatible {
			status.Herdr = HerdrHealthy
		} else {
			status.Herdr = HerdrIncompatible
		}
		if status.State == StateHerdrGrace {
			switch {
			case status.WorkerPID > 0:
				status.State = StateRunning
			case stateBeforeGrace == StateStarting:
				status.State = StateStarting
			default:
				status.State = StateWorkerBackoff
			}
		}
	})
}

func (monitor *HerdrMonitor) markUnavailable() {
	monitor.status.Update(func(status *Status) {
		status.Herdr = HerdrUnavailable
		if status.State != StateStopping {
			status.State = StateHerdrGrace
		}
	})
}

func waitMonitorDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func monitorProbeErrorType(err error) string {
	if err == nil {
		return "unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "context"
	}
	return "unavailable"
}
