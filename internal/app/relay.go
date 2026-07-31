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
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
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
	if err := relay.SetExecutor(service); err != nil {
		return fmt.Errorf("绑定 Relay 执行器: %w", err)
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
	)
	runErr, timedOutComponentsDone = runRelayComponents(ctx, relay.Run, supervisor.Run, defaultShutdownTimeout)
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

func runRelayComponents(ctx context.Context, runRelay, runSupervisor func(context.Context) error, shutdownTimeout time.Duration) (error, <-chan struct{}) {
	componentContext, cancel := context.WithCancel(ctx)
	results := make(chan componentResult, 2)
	done := make(chan struct{})
	go runComponent(componentContext, "relay_connection", true, runRelay, results)
	go runComponent(componentContext, "herdr_supervisor", true, runSupervisor, results)
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
