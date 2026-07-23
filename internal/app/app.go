// Package app 负责装配 Herdr Pal 运行时并协调其生命周期。
package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	dedupeTTL              = 24 * time.Hour
	dedupeCapacity         = 10000
)

var (
	// ErrConfig 表示应用启动参数或配置无效。
	ErrConfig = errors.New("应用配置错误")
	// ErrShutdownTimeout 表示部分运行循环未能在退出上限内停止。
	ErrShutdownTimeout = errors.New("优雅退出超时")
	// ErrLoopStopped 表示常驻运行循环在未取消时意外结束。
	ErrLoopStopped = errors.New("运行循环意外结束")
)

// Options 是启动 Herdr Pal 所需的外部选项。
type Options struct {
	// ConfigPath 是非密钥 JSON 配置文件路径。
	ConfigPath string
	// Getenv 读取企业微信 Secret；nil 时使用 os.Getenv。
	Getenv func(string) string
	// Runner 调用 Herdr 公共 CLI 解析 Socket；nil 时使用本机命令执行器。
	Runner herdr.CommandRunner
	// Stdout 保留给应用标准输出；nil 时丢弃。
	Stdout io.Writer
	// Stderr 接收结构化运行日志；nil 时使用 os.Stderr。
	Stderr io.Writer

	dependencies *appDependencies
}

type processLock interface {
	Release() error
}

type weComRuntime interface {
	bridge.IMAdapter
	Run(ctx context.Context) error
	Events() <-chan wecom.IncomingText
}

type runtimeRunner interface {
	Run(ctx context.Context) error
}

type messageHandler interface {
	HandleMessage(ctx context.Context, message wecom.IncomingText)
}

type applicationRuntime struct {
	wecom      weComRuntime
	supervisor runtimeRunner
	handler    messageHandler

	herdr    bridge.ManagedHerdr
	factory  bridge.HerdrFactory
	service  *bridge.Service
	notifier *bridge.Notifier
}

type appDependencies struct {
	loadConfig      func(string, func(string) string) (config.Config, error)
	userCacheDir    func() (string, error)
	mkdirAll        func(string, os.FileMode) error
	acquireLock     func(string) (processLock, error)
	resolveSocket   func(context.Context, string, string, herdr.CommandRunner) (string, error)
	assemble        func(config.Config, string, *slog.Logger) (*applicationRuntime, error)
	shutdownTimeout time.Duration
}

type assemblyDependencies struct {
	newHerdr func(string) bridge.ManagedHerdr
	newWeCom func(wecom.ClientConfig) (weComRuntime, error)
}

type staticHerdrFactory struct {
	client bridge.ManagedHerdr
}

func (f staticHerdrFactory) Connect(ctx context.Context) (bridge.ManagedHerdr, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.client, nil
}

type commandRunner struct{}

func (commandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Run 加载配置、获取进程锁并运行 bridge，直到 context 取消或运行循环失败。
func Run(ctx context.Context, options Options) (runErr error) {
	if ctx == nil {
		return fmt.Errorf("%w: context 不能为空", ErrConfig)
	}
	if strings.TrimSpace(options.ConfigPath) == "" {
		return fmt.Errorf("%w: 缺少 -config", ErrConfig)
	}
	dependencies := options.dependencies
	if dependencies == nil {
		dependencies = defaultAppDependencies()
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}

	loaded, err := dependencies.loadConfig(options.ConfigPath, getenv)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	logger, err := newLogger(stderr, loaded.Log.Level)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}

	cacheDir, err := dependencies.userCacheDir()
	if err != nil {
		return fmt.Errorf("获取缓存目录: %w", err)
	}
	lockDir := filepath.Join(cacheDir, "herdr-pal")
	if err := dependencies.mkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("创建锁目录: %w", err)
	}
	lockPath := filepath.Join(lockDir, shortHash(loaded.WeCom.BotID)+".lock")
	lock, err := dependencies.acquireLock(lockPath)
	if err != nil {
		return fmt.Errorf("获取进程锁: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			releaseErr := fmt.Errorf("释放进程锁: %w", err)
			if runErr == nil {
				runErr = releaseErr
			} else {
				runErr = errors.Join(runErr, releaseErr)
			}
		}
	}()

	socketPath, err := dependencies.resolveSocket(ctx, loaded.Herdr.SocketPath, loaded.Herdr.Session, runner)
	if err != nil {
		return fmt.Errorf("解析 Herdr Socket: %w", err)
	}
	runtime, err := dependencies.assemble(loaded, socketPath, logger)
	if err != nil {
		return fmt.Errorf("组装应用: %w", err)
	}
	if runtime == nil || runtime.wecom == nil || runtime.supervisor == nil || runtime.handler == nil {
		return errors.New("组装应用: 运行时依赖无效")
	}

	logger.Info("Herdr Pal 启动", "bot_hash", shortHash(loaded.WeCom.BotID), "user_hash", shortHash(loaded.WeCom.AllowedUserID))
	runErr = runRuntime(ctx, runtime, dependencies.shutdownTimeout)
	if runErr != nil {
		logger.Error("Herdr Pal 停止", "error_type", safeErrorType(runErr))
	} else {
		logger.Info("Herdr Pal 已停止")
	}
	return runErr
}

func defaultAppDependencies() *appDependencies {
	return &appDependencies{
		loadConfig:    config.Load,
		userCacheDir:  os.UserCacheDir,
		mkdirAll:      os.MkdirAll,
		acquireLock:   func(path string) (processLock, error) { return processlock.Acquire(path) },
		resolveSocket: herdr.ResolveSocket,
		assemble: func(loaded config.Config, socketPath string, logger *slog.Logger) (*applicationRuntime, error) {
			return assembleRuntime(loaded, socketPath, logger, defaultAssemblyDependencies())
		},
		shutdownTimeout: defaultShutdownTimeout,
	}
}

func defaultAssemblyDependencies() assemblyDependencies {
	return assemblyDependencies{
		newHerdr: func(socketPath string) bridge.ManagedHerdr {
			return herdr.NewClient(socketPath, nil, 0)
		},
		newWeCom: func(clientConfig wecom.ClientConfig) (weComRuntime, error) {
			return wecom.NewClient(clientConfig)
		},
	}
}

func assembleRuntime(loaded config.Config, socketPath string, _ *slog.Logger, dependencies assemblyDependencies) (*applicationRuntime, error) {
	client := dependencies.newHerdr(socketPath)
	if client == nil {
		return nil, errors.New("Herdr Client 无效")
	}
	im, err := dependencies.newWeCom(wecom.ClientConfig{
		Endpoint:      wecom.DefaultEndpoint,
		BotID:         loaded.WeCom.BotID,
		Secret:        loaded.WeCom.Secret,
		AllowedUserID: loaded.WeCom.AllowedUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("创建企业微信客户端: %w", err)
	}
	registry := &session.Registry{}
	buffer := &panel.Buffer{}
	guard, err := policy.NewGuard(loaded.WeCom.AllowedUserID)
	if err != nil {
		return nil, fmt.Errorf("创建输入策略: %w", err)
	}
	deduper, err := policy.NewDeduper(dedupeTTL, dedupeCapacity, time.Now)
	if err != nil {
		return nil, fmt.Errorf("创建消息幂等器: %w", err)
	}
	service, err := bridge.NewService(registry, buffer, guard, deduper, im)
	if err != nil {
		return nil, fmt.Errorf("创建 BridgeService: %w", err)
	}
	notifier, err := bridge.NewNotifier(im, client.GetAgent, client.ReadRecent)
	if err != nil {
		return nil, fmt.Errorf("创建状态通知器: %w", err)
	}
	factory := staticHerdrFactory{client: client}
	supervisor, err := bridge.NewSupervisor(factory, registry, service, notifier, bridge.SupervisorOptions{})
	if err != nil {
		return nil, fmt.Errorf("创建 Herdr Supervisor: %w", err)
	}
	return &applicationRuntime{
		wecom: im, supervisor: supervisor, handler: service,
		herdr: client, factory: factory, service: service, notifier: notifier,
	}, nil
}

type componentResult struct {
	name string
	err  error
}

func runRuntime(parent context.Context, runtime *applicationRuntime, shutdownTimeout time.Duration) error {
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	base := context.WithoutCancel(parent)
	componentContext, cancelComponents := context.WithCancel(base)
	messageContext, stopMessages := context.WithCancel(parent)
	defer cancelComponents()
	defer stopMessages()
	results := make(chan componentResult, 3)
	start := func(name string, run func(context.Context) error) {
		go func() { results <- componentResult{name: name, err: run(componentContext)} }()
	}
	start("wecom", runtime.wecom.Run)
	start("herdr", runtime.supervisor.Run)
	go func() {
		results <- componentResult{name: "messages", err: consumeMessages(messageContext, runtime.wecom.Events(), runtime.handler)}
	}()

	completed := 0
	var firstErr error
	normalCancellation := false
	select {
	case <-parent.Done():
		normalCancellation = true
	case result := <-results:
		completed++
		if parent.Err() != nil {
			normalCancellation = true
		} else if result.err == nil {
			firstErr = fmt.Errorf("%w: %s", ErrLoopStopped, result.name)
		} else {
			firstErr = fmt.Errorf("%s 运行失败: %w", result.name, result.err)
		}
	}
	// 先停止业务消息消费，再取消两侧连接和未完成请求。
	stopMessages()
	cancelComponents()

	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	for completed < 3 {
		select {
		case <-results:
			completed++
		case <-timer.C:
			if firstErr != nil {
				return errors.Join(firstErr, ErrShutdownTimeout)
			}
			return ErrShutdownTimeout
		}
	}
	if normalCancellation {
		return nil
	}
	return firstErr
}

func consumeMessages(ctx context.Context, events <-chan wecom.IncomingText, handler messageHandler) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message, ok := <-events:
			if !ok {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			handler.HandleMessage(ctx, message)
		}
	}
}

func newLogger(destination io.Writer, level string) (*slog.Logger, error) {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		parsed = slog.LevelInfo
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		return nil, fmt.Errorf("log.level 不受支持")
	}
	return slog.New(slog.NewTextHandler(destination, &slog.HandlerOptions{Level: parsed})), nil
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}

func safeErrorType(err error) string {
	if err == nil {
		return "<nil>"
	}
	return reflect.TypeOf(err).String()
}
