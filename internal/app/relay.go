package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/dsh"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/terminalimage"
	"github.com/wenxichang/herdr-pal/internal/version"
)

// RunCLI 按公开 CLI 模式运行本地交互或 Relay 客户端。
func RunCLI(ctx context.Context, options Options) error {
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
	if loaded.Herdr.Disabled {
		return runDSHRelay(ctx, options.ConfigPath, loaded, logger)
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	socketPath, err := herdr.ResolveSocketWithEnvironment(
		ctx,
		loaded.Herdr.SocketPath,
		strings.TrimSpace(getenv("HERDR_SOCKET_PATH")),
		loaded.Herdr.Session,
		runner,
	)
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
	// 混合模式同样运行 dsh 后端，必须持有与 dsh-only 相同的进程锁，避免两个
	// 实例同时控制同一 dsh 宿主。锁持有到整个 Relay 进程退出。
	if loaded.DSH != nil {
		dshLock, err := processlock.Acquire(filepath.Join(lockDir, dshLockName(*loaded.DSH)+".lock"))
		if err != nil {
			return fmt.Errorf("获取 dsh 进程锁（同一 dsh 宿主可能已被其他实例控制）: %w", err)
		}
		defer func() { finishProcessLock(dshLock, &runErr, timedOutComponentsDone) }()
	}
	stableDialPath, err := prepareStableDialPath(canonicalEndpoint)
	if err != nil {
		return fmt.Errorf("准备 Herdr Socket 连接路径: %w", err)
	}
	defer func() { finishDialPathLease(stableDialPath, &runErr, timedOutComponentsDone, logger) }()

	herdrClient := herdr.NewClient(stableDialPath.Path(), nil, 0)
	relayLogger := logger.With("component", "relay_connection", "credential_id", loaded.Relay.CredentialID)
	relay, err := relayclient.New(relayclient.Config{
		URL: loaded.Relay.URL, Key: loaded.Relay.Key, CredentialID: loaded.Relay.CredentialID,
		SkipVerify: loaded.Relay.SkipVerify, Version: version.String(), Logger: relayLogger,
	})
	if err != nil {
		return fmt.Errorf("创建 Relay 客户端: %w", err)
	}
	registry := &session.Registry{}
	guard, err := policy.NewGuard(strconv.FormatUint(loaded.Relay.CredentialID, 10))
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
	renderer, err := newRelayTerminalRenderer()
	if err != nil {
		return fmt.Errorf("创建终端图片渲染器: %w", err)
	}
	if err := service.SetTerminalRenderer(renderer); err != nil {
		return fmt.Errorf("绑定终端图片渲染器: %w", err)
	}
	notifier, err := bridge.NewNotifier(relay, herdrClient.GetAgent)
	if err != nil {
		return fmt.Errorf("创建状态通知器: %w", err)
	}
	supervisor, err := bridge.NewSupervisor(staticHerdrFactory{client: herdrClient}, registry, service, notifier, bridge.SupervisorOptions{
		Logger: logger.With("component", "herdr_supervisor", "credential_id", loaded.Relay.CredentialID),
	})
	if err != nil {
		return fmt.Errorf("创建 Herdr Supervisor: %w", err)
	}
	var executor relayclient.Executor = service
	runSupervisor := supervisor.Run
	var dshBackend *dsh.Backend
	if loaded.DSH != nil {
		backend, err := newDSHBackend(*loaded.DSH, relay, guard, logger.With("component", "dsh_backend", "credential_id", loaded.Relay.CredentialID))
		if err != nil {
			return fmt.Errorf("创建 dsh 后端: %w", err)
		}
		dshBackend = backend
		executor = relayclient.NewExecutorMux(service, backend)
		runSupervisor = func(ctx context.Context) error { return runBackendPair(ctx, supervisor.Run, backend.Run) }
	}
	if err := relay.SetExecutor(executor); err != nil {
		return fmt.Errorf("绑定 Relay 执行器: %w", err)
	}

	herdrSession := strings.TrimSpace(loaded.Herdr.Session)
	if herdrSession == "" {
		herdrSession = "auto"
	}
	logger.Info("Herdr Pal Relay 客户端启动",
		"client_version", version.String(),
		"credential_id", loaded.Relay.CredentialID,
		"relay_endpoint", relayLogEndpoint(loaded.Relay.URL),
		"skip_verify", loaded.Relay.SkipVerify,
		"herdr_session", herdrSession,
		"socket_hash", socketHash,
		"dsh_variant", dshVariantForConfig(loaded.DSH),
	)
	// 混合与纯 dsh 模式都要监听配置：dsh 端口变化后热更新发现配置，无需重启。
	var extras []func(context.Context) error
	if dshBackend != nil {
		extras = append(extras, newDSHConfigWatch(options.ConfigPath, dshBackend, logger))
	}
	runErr, timedOutComponentsDone = runRelayComponents(ctx, relay.Run, runSupervisor, defaultShutdownTimeout, extras...)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		logger.Error("Herdr Pal Relay 客户端停止",
			"component", relayComponentName(runErr),
			"error_type", safeErrorType(runErr),
			"reason", safeRelayRuntimeReason(runErr, loaded.Relay.Key, loaded.Relay.URL),
		)
	}
	return runErr
}

func newRelayTerminalRenderer() (bridge.TerminalRenderer, error) {
	return terminalimage.New()
}

// runRelayComponents 并行运行 relay 与 supervisor，并可选运行其他附属组件。
//
// 前两个组件是关键的：任一结束都会取消其余组件并决定整体生命周期。extra 中
// 的组件是非关键的，它以任何方式结束都只记录日志，不取消关键组件；等待关键
// 组件结果时，它已结束的事实会被忽略。返回的 done 只表示全部组件都已退出。
func runRelayComponents(ctx context.Context, runRelay, runSupervisor func(context.Context) error, shutdownTimeout time.Duration, extra ...func(context.Context) error) (error, <-chan struct{}) {
	componentContext, cancel := context.WithCancel(ctx)
	// 关键组件结束时下面会主动 cancel；defer 只兜住提前返回的路径，避免 context 泄漏。
	defer cancel()
	total := 2 + len(extra)
	// 缓冲区等于组件数，runComponent 的发送永不阻塞。
	results := make(chan componentResult, total)
	done := make(chan struct{})

	var started sync.WaitGroup
	started.Add(total)
	start := func(name string, primary bool, run func(context.Context) error) {
		go func() {
			defer started.Done()
			runComponent(componentContext, name, primary, run, results)
		}()
	}
	start("relay_connection", true, runRelay)
	start("herdr_supervisor", true, runSupervisor)
	for index, run := range extra {
		start(fmt.Sprintf("auxiliary_%d", index), false, run)
	}

	// 结果收集必须在独立 goroutine 中进行：关键组件结束后，其余组件可能卡住，
	// 主流程要能在 shutdownTimeout 到期时先返回，而不是同步等待全部结果。
	//
	// primaryResult 由第一个结束的关键组件写入并关闭，附属组件的结果被忽略。
	primaryResult := make(chan componentResult, 1)
	go func() {
		// 缓冲区等于组件数，runComponent 的发送永不阻塞，因此这里总能收齐。
		remaining := total
		reported := false
		for remaining > 0 {
			result := <-results
			remaining--
			if !result.primary || reported {
				continue
			}
			reported = true
			primaryResult <- result
			close(primaryResult)
			cancel()
		}
	}()
	go func() {
		started.Wait()
		close(done)
	}()
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()

	// 等待第一个结束的关键组件；正常运行时它会一直阻塞在这里，直到进程收到退出信号。
	var first componentResult
	select {
	case first = <-primaryResult:
	case <-ctx.Done():
		// 父 context 结束：等待其余组件收尾，超时则上报。
		select {
		case <-done:
			return ctx.Err(), nil
		case <-timer.C:
			return ctx.Err(), done
		}
	}

	// 关键组件已结束，继续等待其余组件在宽限期内退出。
	timer.Reset(shutdownTimeout)
	select {
	case <-done:
		if ctx.Err() != nil {
			return ctx.Err(), nil
		}
		return relayComponentRootError(first), nil
	case <-timer.C:
		if rootErr := relayComponentRootError(first); rootErr != nil {
			return errors.Join(rootErr, ErrShutdownTimeout), done
		}
		return ErrShutdownTimeout, done
	}
}

type relayComponentError struct {
	component string
	err       error
}

func (e *relayComponentError) Error() string {
	return fmt.Sprintf("%s 运行失败: %v", e.component, e.err)
}

func (e *relayComponentError) Unwrap() error { return e.err }

func relayComponentRootError(result componentResult) error {
	if result.shutdownDerived {
		return nil
	}
	err := result.err
	if err == nil {
		err = ErrLoopStopped
	}
	return &relayComponentError{component: result.name, err: err}
}

func relayComponentName(err error) string {
	var componentErr *relayComponentError
	if errors.As(err, &componentErr) {
		return componentErr.component
	}
	if errors.Is(err, ErrShutdownTimeout) {
		return "shutdown"
	}
	return "unknown"
}

func safeRelayRuntimeReason(err error, secret, endpoint string) string {
	if err == nil {
		return ""
	}
	reason := err.Error()
	if secret = strings.TrimSpace(secret); secret != "" {
		reason = strings.ReplaceAll(reason, secret, "[relay-key]")
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		reason = strings.ReplaceAll(reason, endpoint, relayLogEndpoint(endpoint))
	}
	reason = strings.Join(strings.Fields(reason), " ")
	const maxRunes = 512
	runes := []rune(reason)
	if len(runes) > maxRunes {
		reason = string(runes[:maxRunes]) + "..."
	}
	return reason
}

func relayLogEndpoint(raw string) string {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "invalid"
	}
	return endpoint.Scheme + "://" + endpoint.Host
}
