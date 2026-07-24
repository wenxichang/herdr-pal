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
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/interactive"
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
	maxPureErrorTreeDepth  = 64
	// 预留 pathname 终止符并兼容地址空间更小的 Unix 实现。
	unixSocketPathByteLimit = 96
	stableDialAliasPattern  = "herdr-pal-"
	stableDialAliasLinkName = "s"
)

var (
	// ErrConfig 表示应用启动参数或配置无效。
	ErrConfig = errors.New("应用配置错误")
	// ErrShutdownTimeout 表示部分运行循环未能在退出上限内停止。
	ErrShutdownTimeout = errors.New("优雅退出超时")
	// ErrLoopStopped 表示常驻运行循环在未取消时意外结束。
	ErrLoopStopped                = errors.New("运行循环意外结束")
	errSocketPathUnresolvable     = errors.New("Herdr Socket 路径无法可靠规范化")
	errSocketSymlinkUnresolvable  = errors.New("Herdr Socket symlink 无法可靠解析")
	errStableDialAliasUnavailable = errors.New("Herdr Socket 短连接路径不可用")
	errStableDialPathTooLong      = errors.New("Herdr Socket 连接路径超过安全长度")
	errStableDialAliasCleanup     = errors.New("清理 Herdr Socket 短连接路径失败")

	// 超时返回时运行循环可能仍持有网络资源，因此必须继续持有文件锁，避免同一进程内
	// 的 finalizer 或 GC 间接解锁并允许第二个实例启动。该集合只接管超时锁；循环永久
	// 卡住时持有至进程退出，全部退出后再安全释放。
	retainedLocksMu sync.Mutex
	retainedLocks   []*retainedProcessLock
)

// Options 是启动 Herdr Pal 所需的外部选项。
type Options struct {
	// Interactive 选择使用本机标准输入输出的交互模式。
	Interactive bool
	// ConfigPath 是非密钥 JSON 配置文件路径。
	ConfigPath string
	// Stdin 是交互模式输入；nil 时使用 os.Stdin。
	Stdin io.Reader
	// Getenv 读取企业微信 Secret；nil 时使用 os.Getenv。
	Getenv func(string) string
	// Runner 调用 Herdr 公共 CLI 解析 Socket；nil 时使用本机命令执行器。
	Runner herdr.CommandRunner
	// Stdout 是应用标准输出；交互模式下 nil 使用 os.Stdout，普通模式下 nil 丢弃。
	Stdout io.Writer
	// Stderr 接收结构化运行日志；nil 时使用 os.Stderr。
	Stderr io.Writer
	// WeComEndpoint 覆盖企业微信长连接地址；空值使用官方端点。该入口用于兼容端点和集成测试，CLI 与配置文件不暴露它。
	WeComEndpoint string

	dependencies *appDependencies
}

type processLock interface {
	Release() error
}

type retainedProcessLock struct {
	lock processLock
}

// dialPathLease 将短别名生命周期绑定到仍可能重连的运行组件。
type dialPathLease struct {
	path      string
	aliasDir  string
	aliasLink string
	closeOnce sync.Once
	closeErr  error
}

func (l *dialPathLease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *dialPathLease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.aliasDir == "" {
			return
		}
		failed := false
		if err := os.Remove(l.aliasLink); err != nil && !os.IsNotExist(err) {
			failed = true
		}
		if err := os.Remove(l.aliasDir); err != nil && !os.IsNotExist(err) {
			failed = true
		}
		if failed {
			l.closeErr = errStableDialAliasCleanup
		}
	})
	return l.closeErr
}

type imRuntime interface {
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
	im           imRuntime
	supervisor   runtimeRunner
	handler      messageHandler
	normalIMExit func(error) bool

	herdr    bridge.ManagedHerdr
	factory  bridge.HerdrFactory
	service  *bridge.Service
	notifier *bridge.Notifier
}

type appDependencies struct {
	loadConfig            func(string, func(string) string) (config.Config, error)
	loadInteractiveConfig func(string) (config.Config, error)
	userCacheDir          func() (string, error)
	mkdirAll              func(string, os.FileMode) error
	acquireLock           func(string) (processLock, error)
	resolveSocket         func(context.Context, string, string, herdr.CommandRunner) (string, error)
	prepareStableDialPath func(string) (*dialPathLease, error)
	assemble              func(config.Config, string, *slog.Logger) (*applicationRuntime, error)
	assembleInteractive   func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error)
	shutdownTimeout       time.Duration
}

type assemblyDependencies struct {
	newHerdr       func(string) bridge.ManagedHerdr
	newWeCom       func(wecom.ClientConfig) (imRuntime, error)
	newInteractive func(io.Reader, io.Writer) (imRuntime, error)
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

// Run 按选项选择运行模式，直到 context 取消或运行循环失败。
func Run(ctx context.Context, options Options) error {
	if options.Interactive {
		return runInteractive(ctx, options)
	}
	return runWeCom(ctx, options)
}

func runWeCom(ctx context.Context, options Options) (runErr error) {
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
	loaded.WeCom.Endpoint = strings.TrimSpace(options.WeComEndpoint)
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
	var timedOutComponentsDone <-chan struct{}
	defer func() {
		finishProcessLock(lock, &runErr, timedOutComponentsDone)
	}()

	socketPath, err := dependencies.resolveSocket(ctx, loaded.Herdr.SocketPath, loaded.Herdr.Session, runner)
	if err != nil {
		return fmt.Errorf("解析 Herdr Socket: %w", err)
	}
	runtime, err := dependencies.assemble(loaded, socketPath, logger)
	if err != nil {
		return fmt.Errorf("组装应用: %w", err)
	}
	if runtime == nil || runtime.im == nil || runtime.supervisor == nil || runtime.handler == nil {
		return errors.New("组装应用: 运行时依赖无效")
	}

	logger.Info("Herdr Pal 启动", "bot_hash", shortHash(loaded.WeCom.BotID), "user_hash", shortHash(loaded.WeCom.AllowedUserID))
	outcome := runRuntime(ctx, runtime, dependencies.shutdownTimeout)
	runErr = outcome.err
	timedOutComponentsDone = outcome.componentsDone
	if runErr != nil {
		logger.Error("Herdr Pal 停止", "error_type", safeErrorType(runErr))
	} else {
		logger.Info("Herdr Pal 已停止")
	}
	return runErr
}

func runInteractive(ctx context.Context, options Options) (runErr error) {
	if ctx == nil {
		return fmt.Errorf("%w: context 不能为空", ErrConfig)
	}
	dependencies := options.dependencies
	if dependencies == nil {
		dependencies = defaultAppDependencies()
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	stdin := options.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	loaded, err := dependencies.loadInteractiveConfig(options.ConfigPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	logger, err := newLogger(stderr, loaded.Log.Level)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	socketPath, err := dependencies.resolveSocket(ctx, loaded.Herdr.SocketPath, loaded.Herdr.Session, runner)
	if err != nil {
		return fmt.Errorf("解析 Herdr Socket: %w", err)
	}
	canonicalEndpoint, err := canonicalSocketPath(socketPath)
	if err != nil {
		return fmt.Errorf("规范化 Herdr Socket: %w", err)
	}
	socketHash := shortHash(canonicalEndpoint)
	cacheDir, err := dependencies.userCacheDir()
	if err != nil {
		return fmt.Errorf("获取缓存目录: %w", err)
	}
	lockDir := filepath.Join(cacheDir, "herdr-pal")
	if err := dependencies.mkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("创建锁目录: %w", err)
	}
	lockPath := filepath.Join(lockDir, "interactive-"+socketHash+".lock")
	lock, err := dependencies.acquireLock(lockPath)
	if err != nil {
		return fmt.Errorf("获取进程锁: %w", err)
	}
	var timedOutComponentsDone <-chan struct{}
	defer func() {
		finishProcessLock(lock, &runErr, timedOutComponentsDone)
	}()
	stableDialPath, err := dependencies.prepareStableDialPath(canonicalEndpoint)
	if err != nil {
		return fmt.Errorf("准备 Herdr Socket 连接路径: %w", err)
	}
	defer func() {
		finishDialPathLease(stableDialPath, &runErr, timedOutComponentsDone, logger)
	}()

	runtime, err := dependencies.assembleInteractive(loaded, stableDialPath.Path(), stdin, stdout, logger)
	if err != nil {
		return fmt.Errorf("组装应用: %w", err)
	}
	if runtime == nil || runtime.im == nil || runtime.supervisor == nil || runtime.handler == nil {
		return errors.New("组装应用: 运行时依赖无效")
	}

	logger.Info("Herdr Pal 启动", "mode", "interactive", "socket_hash", socketHash)
	outcome := runRuntime(ctx, runtime, dependencies.shutdownTimeout)
	runErr = outcome.err
	timedOutComponentsDone = outcome.componentsDone
	if runErr != nil {
		logger.Error("Herdr Pal 停止", "error_type", safeErrorType(runErr))
	} else {
		logger.Info("Herdr Pal 已停止", "mode", "interactive", "socket_hash", socketHash)
	}
	return runErr
}

// canonicalSocketPath 冻结交互锁和日志使用的端点身份，避免 symlink 别名和重定向漂移。
func canonicalSocketPath(socketPath string) (string, error) {
	if strings.TrimSpace(socketPath) == "" {
		return "", errors.New("Herdr Socket 路径不能为空")
	}
	absolute, err := filepath.Abs(filepath.Clean(socketPath))
	if err != nil {
		return "", errSocketPathUnresolvable
	}

	resolved, resolveErr := filepath.EvalSymlinks(absolute)
	if resolveErr == nil {
		return filepath.Clean(resolved), nil
	}

	info, lstatErr := os.Lstat(absolute)
	if lstatErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errSocketSymlinkUnresolvable
		}
		return "", errSocketPathUnresolvable
	}
	if !os.IsNotExist(resolveErr) || !os.IsNotExist(lstatErr) {
		return "", errSocketPathUnresolvable
	}

	// 仅允许 Socket leaf 尚未创建；父目录身份必须在加锁前完整冻结。
	parent := filepath.Dir(absolute)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errSocketPathUnresolvable
	}
	parentInfo, err := os.Stat(resolvedParent)
	if err != nil || !parentInfo.IsDir() {
		return "", errSocketPathUnresolvable
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

// prepareStableDialPath 为过长端点创建进程私有短别名，同时保持 canonical endpoint 身份不变。
func prepareStableDialPath(canonicalEndpoint string) (*dialPathLease, error) {
	if len([]byte(canonicalEndpoint)) < unixSocketPathByteLimit {
		return &dialPathLease{path: canonicalEndpoint}, nil
	}

	aliasDir, err := os.MkdirTemp("/tmp", stableDialAliasPattern)
	if err != nil {
		return nil, errStableDialAliasUnavailable
	}
	if err := os.Chmod(aliasDir, 0o700); err != nil {
		_ = os.Remove(aliasDir)
		return nil, errStableDialAliasUnavailable
	}
	aliasLink := filepath.Join(aliasDir, stableDialAliasLinkName)
	if err := os.Symlink(canonicalEndpoint, aliasLink); err != nil {
		_ = os.Remove(aliasDir)
		return nil, errStableDialAliasUnavailable
	}
	lease := &dialPathLease{
		path:      aliasLink,
		aliasDir:  aliasDir,
		aliasLink: aliasLink,
	}
	if err := validateUnixSocketDialPath(lease.path); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func validateUnixSocketDialPath(dialPath string) error {
	if len([]byte(dialPath)) >= unixSocketPathByteLimit {
		return errStableDialPathTooLong
	}
	return nil
}

func finishDialPathLease(lease *dialPathLease, runErr *error, timedOutComponentsDone <-chan struct{}, logger *slog.Logger) {
	if errors.Is(*runErr, ErrShutdownTimeout) {
		retainDialPathLeaseUntilDone(lease, timedOutComponentsDone, logger)
		return
	}
	if err := lease.Close(); err != nil {
		if *runErr == nil {
			*runErr = err
		} else {
			*runErr = errors.Join(*runErr, err)
		}
	}
}

func retainDialPathLeaseUntilDone(lease *dialPathLease, componentsDone <-chan struct{}, logger *slog.Logger) {
	if lease == nil || componentsDone == nil {
		return
	}
	go func() {
		<-componentsDone
		if err := lease.Close(); err != nil && logger != nil {
			logger.Error("Herdr Socket 短连接路径清理失败", "error_type", safeErrorType(err))
		}
	}()
}

func finishProcessLock(lock processLock, runErr *error, timedOutComponentsDone <-chan struct{}) {
	if errors.Is(*runErr, ErrShutdownTimeout) {
		retainProcessLockUntilDone(lock, timedOutComponentsDone)
		return
	}
	if err := lock.Release(); err != nil {
		releaseErr := fmt.Errorf("释放进程锁: %w", err)
		if *runErr == nil {
			*runErr = releaseErr
		} else {
			*runErr = errors.Join(*runErr, releaseErr)
		}
	}
}

func defaultAppDependencies() *appDependencies {
	return &appDependencies{
		loadConfig:            config.Load,
		loadInteractiveConfig: config.LoadInteractive,
		userCacheDir:          os.UserCacheDir,
		mkdirAll:              os.MkdirAll,
		acquireLock:           func(path string) (processLock, error) { return processlock.Acquire(path) },
		resolveSocket:         herdr.ResolveSocket,
		prepareStableDialPath: prepareStableDialPath,
		assemble: func(loaded config.Config, socketPath string, logger *slog.Logger) (*applicationRuntime, error) {
			return assembleRuntime(loaded, socketPath, logger, defaultAssemblyDependencies())
		},
		assembleInteractive: func(loaded config.Config, socketPath string, input io.Reader, output io.Writer, logger *slog.Logger) (*applicationRuntime, error) {
			return assembleInteractiveRuntime(loaded, socketPath, input, output, logger, defaultAssemblyDependencies())
		},
		shutdownTimeout: defaultShutdownTimeout,
	}
}

func defaultAssemblyDependencies() assemblyDependencies {
	return assemblyDependencies{
		newHerdr: func(socketPath string) bridge.ManagedHerdr {
			return herdr.NewClient(socketPath, nil, 0)
		},
		newWeCom: func(clientConfig wecom.ClientConfig) (imRuntime, error) {
			client, err := wecom.NewClient(clientConfig)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		newInteractive: func(input io.Reader, output io.Writer) (imRuntime, error) {
			return interactive.NewAdapter(input, output)
		},
	}
}

func assembleRuntime(loaded config.Config, socketPath string, logger *slog.Logger, dependencies assemblyDependencies) (*applicationRuntime, error) {
	if logger == nil {
		return nil, errors.New("结构化日志器无效")
	}
	client := dependencies.newHerdr(socketPath)
	if client == nil {
		return nil, errors.New("Herdr Client 无效")
	}
	endpoint := loaded.WeCom.Endpoint
	if endpoint == "" {
		endpoint = wecom.DefaultEndpoint
	}
	im, err := dependencies.newWeCom(wecom.ClientConfig{
		Endpoint:      endpoint,
		BotID:         loaded.WeCom.BotID,
		Secret:        loaded.WeCom.Secret,
		AllowedUserID: loaded.WeCom.AllowedUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("创建企业微信客户端: %w", err)
	}
	return assembleBridgeRuntime(im, loaded.WeCom.AllowedUserID, client, logger)
}

func assembleInteractiveRuntime(_ config.Config, socketPath string, input io.Reader, output io.Writer, logger *slog.Logger, dependencies assemblyDependencies) (*applicationRuntime, error) {
	if logger == nil {
		return nil, errors.New("结构化日志器无效")
	}
	client := dependencies.newHerdr(socketPath)
	if client == nil {
		return nil, errors.New("Herdr Client 无效")
	}
	im, err := dependencies.newInteractive(input, output)
	if err != nil {
		return nil, fmt.Errorf("创建交互适配器: %w", err)
	}
	runtime, err := assembleBridgeRuntime(im, interactive.UserID, client, logger)
	if err != nil {
		return nil, err
	}
	runtime.normalIMExit = func(err error) bool {
		return isPureErrorTree(err, interactive.ErrInputClosed)
	}
	return runtime, nil
}

// isPureErrorTree 只接受每条解包分支都归结为 target 的有限错误树，避免宽松 Is 匹配隐藏联合根因。
func isPureErrorTree(err error, target error) bool {
	return isPureErrorTreeAtDepth(err, target, 0)
}

func isPureErrorTreeAtDepth(err error, target error, depth int) bool {
	if err == nil || target == nil {
		return false
	}
	if err == target {
		return true
	}
	if depth >= maxPureErrorTreeDepth {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureErrorTreeAtDepth(child, target, depth+1) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isPureErrorTreeAtDepth(wrapped.Unwrap(), target, depth+1)
	default:
		return false
	}
}

func assembleBridgeRuntime(im imRuntime, allowedUserID string, client bridge.ManagedHerdr, logger *slog.Logger) (*applicationRuntime, error) {
	if logger == nil {
		return nil, errors.New("结构化日志器无效")
	}
	if im == nil {
		return nil, errors.New("IM Adapter 无效")
	}
	if client == nil {
		return nil, errors.New("Herdr Client 无效")
	}
	registry := &session.Registry{}
	buffer := &panel.Buffer{}
	guard, err := policy.NewGuard(allowedUserID)
	if err != nil {
		return nil, fmt.Errorf("创建输入策略: %w", err)
	}
	deduper, err := policy.NewDeduper(dedupeTTL, dedupeCapacity, time.Now)
	if err != nil {
		return nil, fmt.Errorf("创建消息幂等器: %w", err)
	}
	service, err := bridge.NewService(registry, buffer, guard, deduper, im, newSlogKeyAuditSink(logger))
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
		im: im, supervisor: supervisor, handler: service,
		herdr: client, factory: factory, service: service, notifier: notifier,
	}, nil
}

type slogKeyAuditSink struct {
	logger *slog.Logger
}

func newSlogKeyAuditSink(logger *slog.Logger) slogKeyAuditSink {
	return slogKeyAuditSink{logger: slog.New(auditLogHandler{Handler: logger.Handler()})}
}

func (s slogKeyAuditSink) RecordKeyAudit(audit policy.KeyAudit) {
	s.logger.Info("显式按键审计",
		slog.String("user_id", audit.UserID()),
		slog.String("pane_id", audit.PaneID()),
		slog.String("occupant_hash", audit.OccupantHash()),
		slog.String("key", audit.Key()),
		slog.Time("at", audit.At()),
		slog.String("result", string(audit.Result())),
	)
}

// auditLogHandler 复用应用日志格式和目的地，但不让普通日志级别过滤安全审计。
type auditLogHandler struct {
	slog.Handler
}

func (h auditLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h auditLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return auditLogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h auditLogHandler) WithGroup(name string) slog.Handler {
	return auditLogHandler{Handler: h.Handler.WithGroup(name)}
}

type componentResult struct {
	name            string
	primary         bool
	err             error
	shutdownDerived bool
}

type runtimeOutcome struct {
	err            error
	componentsDone <-chan struct{}
}

type shutdownCause uint8

const (
	shutdownCauseComponent shutdownCause = iota
	shutdownCauseParent
	shutdownCauseNormalIM
)

func runRuntime(parent context.Context, runtime *applicationRuntime, shutdownTimeout time.Duration) runtimeOutcome {
	if parent.Err() != nil {
		return runtimeOutcome{}
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	base := context.WithoutCancel(parent)
	componentContext, cancelComponents := context.WithCancel(base)
	messageContext, stopMessages := context.WithCancel(parent)
	defer cancelComponents()
	defer stopMessages()
	results := make(chan componentResult, 3)
	start := func(name string, primary bool, run func(context.Context) error) {
		go runComponent(componentContext, name, primary, run, results)
	}
	start("im", true, runtime.im.Run)
	start("herdr", true, runtime.supervisor.Run)
	go runComponent(messageContext, "messages", false, func(ctx context.Context) error {
		return consumeMessages(ctx, runtime.im.Events(), runtime.handler)
	}, results)

	collected := make([]componentResult, 0, 3)
	selectedParent := false
	var selectedResult componentResult
	selectedComponent := false
	select {
	case <-parent.Done():
		selectedParent = true
	case result := <-results:
		collected = append(collected, result)
		selectedResult = result
		selectedComponent = true
	}
	cause := shutdownCauseComponent
	if parentTriggeredShutdown(parent, selectedParent, selectedResult, selectedComponent) {
		cause = shutdownCauseParent
	} else if normalIMTriggeredShutdown(runtime, selectedResult, selectedComponent) {
		cause = shutdownCauseNormalIM
	}
	// 先停止业务消息消费，再取消两侧连接和未完成请求。
	stopMessages()
	cancelComponents()

	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	for len(collected) < 3 {
		select {
		case result := <-results:
			collected = append(collected, result)
		case <-timer.C:
			componentsDone := collectRemainingComponents(results, 3-len(collected))
			if rootErr := runtimeRootError(cause, collected); rootErr != nil {
				return runtimeOutcome{err: errors.Join(rootErr, ErrShutdownTimeout), componentsDone: componentsDone}
			}
			return runtimeOutcome{err: ErrShutdownTimeout, componentsDone: componentsDone}
		}
	}
	return runtimeOutcome{err: runtimeRootError(cause, collected)}
}

func parentTriggeredShutdown(parent context.Context, selectedParent bool, selectedResult componentResult, selectedComponent bool) bool {
	if selectedParent {
		return true
	}
	return selectedComponent && selectedResult.shutdownDerived && parent.Err() != nil
}

func normalIMTriggeredShutdown(runtime *applicationRuntime, selectedResult componentResult, selectedComponent bool) bool {
	return runtime != nil && runtime.normalIMExit != nil && selectedComponent &&
		selectedResult.name == "im" && runtime.normalIMExit(selectedResult.err)
}

func runComponent(ctx context.Context, name string, primary bool, run func(context.Context) error, results chan<- componentResult) {
	err := run(ctx)
	contextErr := ctx.Err()
	results <- componentResult{
		name: name, primary: primary, err: err,
		shutdownDerived: contextErr != nil && errors.Is(err, contextErr),
	}
}

func runtimeRootError(cause shutdownCause, results []componentResult) error {
	if cause == shutdownCauseParent {
		return nil
	}
	if cause == shutdownCauseNormalIM {
		return runtimeRootErrorAfterNormalIM(results)
	}
	for _, result := range results {
		if !result.primary || result.err == nil || result.shutdownDerived {
			continue
		}
		return fmt.Errorf("%s 运行失败: %w", result.name, result.err)
	}
	for _, result := range results {
		if result.err == nil {
			return fmt.Errorf("%w: %s", ErrLoopStopped, result.name)
		}
	}
	for _, result := range results {
		if result.err != nil && !result.shutdownDerived {
			return fmt.Errorf("%s 运行失败: %w", result.name, result.err)
		}
	}
	return ErrLoopStopped
}

func runtimeRootErrorAfterNormalIM(results []componentResult) error {
	for _, result := range results {
		if result.name == "im" || !result.primary || result.err == nil || result.shutdownDerived {
			continue
		}
		return fmt.Errorf("%s 运行失败: %w", result.name, result.err)
	}
	for _, result := range results {
		if result.name == "im" || result.primary || result.err == nil || result.shutdownDerived {
			continue
		}
		return fmt.Errorf("%s 运行失败: %w", result.name, result.err)
	}
	return nil
}

func collectRemainingComponents(results <-chan componentResult, remaining int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range remaining {
			<-results
		}
	}()
	return done
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
			done, err := consumeSelectedMessage(ctx, handler, message, ok)
			if done {
				return err
			}
		}
	}
}

func consumeSelectedMessage(ctx context.Context, handler messageHandler, message wecom.IncomingText, ok bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if !ok {
		return true, nil
	}
	handler.HandleMessage(ctx, message)
	return false, nil
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
	switch {
	case errors.Is(err, ErrShutdownTimeout):
		return "shutdown_timeout"
	case errors.Is(err, ErrLoopStopped):
		return "loop_stopped"
	case errors.Is(err, errStableDialAliasCleanup):
		return "socket_path_cleanup"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.Is(err, bridge.ErrNotificationQueueFull):
		return "notification_queue_full"
	case errors.Is(err, herdr.ErrProtocol), errors.Is(err, herdr.ErrProtocolMismatch):
		return "herdr_protocol"
	case errors.Is(err, herdr.ErrUnavailable):
		return "herdr_unavailable"
	case errors.Is(err, wecom.ErrProtocol):
		return "wecom_protocol"
	case errors.Is(err, wecom.ErrUnavailable):
		return "wecom_unavailable"
	default:
		return "runtime_error"
	}
}

func retainProcessLockUntilDone(lock processLock, componentsDone <-chan struct{}) {
	retained := &retainedProcessLock{lock: lock}
	retainedLocksMu.Lock()
	retainedLocks = append(retainedLocks, retained)
	retainedLocksMu.Unlock()
	if componentsDone == nil {
		return
	}
	go func() {
		<-componentsDone
		if err := retained.lock.Release(); err != nil {
			return
		}
		retainedLocksMu.Lock()
		for index, candidate := range retainedLocks {
			if candidate == retained {
				copy(retainedLocks[index:], retainedLocks[index+1:])
				last := len(retainedLocks) - 1
				retainedLocks[last] = nil
				retainedLocks = retainedLocks[:last]
				break
			}
		}
		retainedLocksMu.Unlock()
	}()
}
