package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/version"
)

// RunCLI 按公开 CLI 模式运行本地交互或 Relay 客户端。
func RunCLI(ctx context.Context, options Options) error {
	if options.DiscoverUser {
		return newConfigError("-discover-user 已移除")
	}
	if options.Interactive {
		return Run(ctx, options)
	}
	return RunRelay(ctx, options)
}

// RunRelay 连接本机 Herdr 与中央 Relay Server，直到 context 取消或关键组件失败。
func RunRelay(ctx context.Context, options Options) (runErr error) {
	if ctx == nil {
		return newConfigError("context 不能为空")
	}
	if strings.TrimSpace(options.ConfigPath) == "" {
		return newConfigError("缺少 -config")
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	loaded, err := config.LoadClient(options.ConfigPath)
	if err != nil {
		return newConfigError(err.Error())
	}
	logger, err := newLogger(stderr, loaded.Log.Level)
	if err != nil {
		return newConfigError(err.Error())
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	socketPath, err := herdr.ResolveSocket(ctx, loaded.Herdr.SocketPath, loaded.Herdr.Session, runner)
	if err != nil {
		return fmt.Errorf("解析 Herdr Socket: %w", err)
	}
	canonicalEndpoint, err := canonicalSocketPath(socketPath)
	if err != nil {
		return fmt.Errorf("规范化 Herdr Socket: %w", err)
	}
	socketHash := shortHash(canonicalEndpoint)
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("获取缓存目录: %w", err)
	}
	lockDir := filepath.Join(cacheDir, "herdr-pal")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("创建锁目录: %w", err)
	}
	lock, err := processlock.Acquire(filepath.Join(lockDir, socketHash+".lock"))
	if err != nil {
		return fmt.Errorf("获取进程锁: %w", err)
	}
	var timedOutComponentsDone <-chan struct{}
	defer func() { finishProcessLock(lock, &runErr, timedOutComponentsDone) }()
	stableDialPath, err := prepareStableDialPath(canonicalEndpoint)
	if err != nil {
		return fmt.Errorf("准备 Herdr Socket 连接路径: %w", err)
	}
	defer func() { finishDialPathLease(stableDialPath, &runErr, timedOutComponentsDone, logger) }()

	herdrClient := herdr.NewClient(stableDialPath.Path(), nil, 0)
	relay, err := relayclient.New(relayclient.Config{
		URL: loaded.Relay.URL, UserID: loaded.Relay.UserID, MachineID: loaded.Relay.MachineID,
		SkipVerify: loaded.Relay.SkipVerify, Version: version.String(), Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("创建 Relay 客户端: %w", err)
	}
	registry := &session.Registry{}
	guard, err := policy.NewGuard(loaded.Relay.UserID)
	if err != nil {
		return fmt.Errorf("创建输入策略: %w", err)
	}
	deduper, err := policy.NewDeduper(dedupeTTL, dedupeCapacity, time.Now)
	if err != nil {
		return fmt.Errorf("创建消息幂等器: %w", err)
	}
	service, err := bridge.NewService(registry, &panel.Buffer{}, guard, deduper, relay, newSlogKeyAuditSink(logger), logger)
	if err != nil {
		return fmt.Errorf("创建 BridgeService: %w", err)
	}
	if err := relay.SetExecutor(service); err != nil {
		return fmt.Errorf("绑定 Relay 执行器: %w", err)
	}
	notifier, err := bridge.NewNotifier(relay, herdrClient.GetAgent, herdrClient.ReadRecent)
	if err != nil {
		return fmt.Errorf("创建状态通知器: %w", err)
	}
	supervisor, err := bridge.NewSupervisor(staticHerdrFactory{client: herdrClient}, registry, service, notifier, bridge.SupervisorOptions{
		Logger: logger.With("component", "herdr_supervisor", "machine_id", loaded.Relay.MachineID),
	})
	if err != nil {
		return fmt.Errorf("创建 Herdr Supervisor: %w", err)
	}

	logger.Info("Herdr Pal Relay 客户端启动", "user_hash", shortHash(loaded.Relay.UserID), "machine_id", loaded.Relay.MachineID, "socket_hash", socketHash)
	runErr, timedOutComponentsDone = runRelayComponents(ctx, relay.Run, supervisor.Run, defaultShutdownTimeout)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		logger.Error("Herdr Pal Relay 客户端停止", "error_type", safeErrorType(runErr))
	}
	return runErr
}

func runRelayComponents(ctx context.Context, runRelay, runSupervisor func(context.Context) error, shutdownTimeout time.Duration) (error, <-chan struct{}) {
	componentContext, cancel := context.WithCancel(ctx)
	results := make(chan error, 2)
	done := make(chan struct{})
	go func() { results <- runRelay(componentContext) }()
	go func() { results <- runSupervisor(componentContext) }()
	first := <-results
	cancel()
	go func() {
		<-results
		close(done)
	}()
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
		if ctx.Err() != nil {
			return ctx.Err(), nil
		}
		return first, nil
	case <-timer.C:
		return ErrShutdownTimeout, done
	}
}
