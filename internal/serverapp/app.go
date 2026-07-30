// Package serverapp 负责装配 herdr-pal-server 的企业微信和 Relay 生命周期。
package serverapp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/adminserver"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/lokiquery"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/version"
	"github.com/wenxichang/herdr-pal/internal/webadmin"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

var ErrConfig = errors.New("服务端配置错误")

var _ server.WeComImageGateway = (*wecom.Client)(nil)

// Options 是 herdr-pal-server 的外部启动选项。
type Options struct {
	ConfigPath string
	Getenv     func(string) string
	Stdout     io.Writer
	Stderr     io.Writer
	Verbose    bool
	// AuthFile 覆盖固定管理员认证文件路径；只供本机自动化测试隔离状态。
	AuthFile string
	// WeComEndpoint 覆盖企业微信长连接地址；CLI 和配置文件不暴露该入口。
	WeComEndpoint string
}

// Run 启动唯一企业微信连接和 Relay TLS 监听，直到 context 取消或关键组件失败。
func Run(ctx context.Context, options Options) error {
	if ctx == nil || strings.TrimSpace(options.ConfigPath) == "" {
		return fmt.Errorf("%w: 缺少启动参数", ErrConfig)
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	loaded, err := config.LoadServer(options.ConfigPath, getenv)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(cacheDir, "herdr-pal-server")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	lock, err := processlock.Acquire(filepath.Join(lockDir, shortHash(loaded.WeCom.BotID)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Release()
	authFile := loaded.Admin.AuthFile
	if strings.TrimSpace(options.AuthFile) != "" {
		authFile = filepath.Clean(strings.TrimSpace(options.AuthFile))
	}
	authStore, bootstrap, err := adminauth.Load(authFile, adminauth.Options{})
	if err != nil {
		return fmt.Errorf("加载管理员认证文件 %q: %w", authFile, err)
	}
	runtimeSecrets := []string{loaded.WeCom.Secret, bootstrap.InitialPassword, bootstrap.AutomationToken}
	for _, value := range loaded.Audit.Headers {
		runtimeSecrets = append(runtimeSecrets, value)
	}
	runtimeLogger, err := newLogger(stderr, loaded.Log.Level, options.Verbose, runtimeSecrets...)
	if err != nil {
		if bootstrap.Created {
			_ = printAdminBootstrap(stdout, bootstrap)
		}
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	logger := runtimeLogger.Logger
	if bootstrap.Created {
		if err := printAdminBootstrap(stdout, bootstrap); err != nil {
			return fmt.Errorf("输出初始管理员凭据: %w", err)
		}
	}
	businessAuditor, auditRedactor, err := newBusinessAuditor(loaded.Audit, stderr, logger, loaded.WeCom.Secret, version.Version)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := businessAuditor.Shutdown(shutdownContext); err != nil {
			logger.Warn("关闭业务审计输出超时", "error_type", "audit_shutdown_timeout")
		}
	}()
	tlsBundle, err := server.EnsureTLS(server.TLSConfig{CertFile: loaded.Server.CertFile, KeyFile: loaded.Server.KeyFile, StateDir: loaded.Server.StateDir})
	if err != nil {
		return fmt.Errorf("准备 Relay TLS: %w", err)
	}
	weComEndpoint := strings.TrimSpace(options.WeComEndpoint)
	if weComEndpoint == "" {
		weComEndpoint = wecom.DefaultEndpoint
	}
	weComClient, err := wecom.NewClient(wecom.ClientConfig{
		Endpoint: weComEndpoint, BotID: loaded.WeCom.BotID, Secret: loaded.WeCom.Secret, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("创建企业微信客户端: %w", err)
	}
	credentialStore, err := credential.LoadStore(loaded.Server.CredentialsFile)
	if err != nil {
		return fmt.Errorf("加载 HPRP 凭据存储: %w", err)
	}
	catalog := server.NewSessionCatalog()
	hub, err := server.NewClientHub(catalog, credentialStore, server.HubConfig{}, logger)
	if err != nil {
		return err
	}
	deduper, err := policy.NewDeduper(24*time.Hour, 10000, time.Now)
	if err != nil {
		return err
	}
	relayURL, err := buildRelayURLHint(loaded.Server.AddrHint, loaded.Server.Listen)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	router, err := server.NewConversationRouterWithConfig(
		server.ConversationRouterConfig{
			RelayURL:    relayURL,
			RateLimiter: server.NewUserRateLimiter(loaded.RateLimit.PerSecond, loaded.RateLimit.PerMinute, time.Now),
			Auditor:     businessAuditor, AuditRedactor: auditRedactor, BotIDHash: shortHash(loaded.WeCom.BotID),
		},
		catalog, server.NewUserExecutor(64), weComClient, hub, deduper, logger,
	)
	if err != nil {
		return err
	}
	if err := hub.SetOutboundSink(router); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", loaded.Server.Listen)
	if err != nil {
		return fmt.Errorf("监听 Relay 地址: %w", err)
	}
	defer listener.Close()
	adminListener, err := adminserver.Listen(adminserver.ListenerConfig{StateDir: loaded.Server.StateDir})
	if err != nil {
		return fmt.Errorf("监听 HPAP Admin Socket: %w", err)
	}
	defer adminListener.Close()
	webListener, err := net.Listen("tcp", loaded.Admin.Listen)
	if err != nil {
		return fmt.Errorf("监听 Web 管理地址: %w", err)
	}
	defer webListener.Close()
	relayTLSConfig := tlsBundle.Config.Clone()
	relayHTTPServer := &http.Server{Handler: hub, TLSConfig: relayTLSConfig, ReadHeaderTimeout: 10 * time.Second}
	relayTLSListener := tls.NewListener(listener, relayTLSConfig)

	stopRequested := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() { stopOnce.Do(func() { close(stopRequested) }) }
	runtimeInspector, err := NewRuntimeInspector(RuntimeConfig{
		StartedAt: time.Now(), RelayListen: listener.Addr().String(),
		AdminSocket: loaded.Server.AdminSocketPath, WebAdminListen: webListener.Addr().String(),
		TLS: tlsBundle.Info, Stop: requestStop,
	}, runtimeLogger, weComClient, hub, catalog, credentialStore)
	if err != nil {
		return fmt.Errorf("创建服务运行状态: %w", err)
	}
	adminService, err := adminservice.New(adminservice.Config{
		Credentials: credentialStore,
		Connections: hub,
		Sessions:    catalog,
		Runtime:     runtimeInspector,
		Now:         time.Now,
	})
	if err != nil {
		return fmt.Errorf("创建共享管理服务: %w", err)
	}
	adminSessions, err := adminauth.NewSessionManager(adminauth.SessionConfig{})
	if err != nil {
		return fmt.Errorf("创建管理员会话管理器: %w", err)
	}
	var auditQuerier webadmin.AuditQuerier
	if loaded.Admin.LokiURL != "" {
		auditQuerier, err = lokiquery.New(lokiquery.Config{BaseURL: loaded.Admin.LokiURL})
		if err != nil {
			return fmt.Errorf("%w: 创建 Loki 查询客户端: %v", ErrConfig, err)
		}
	}
	webRuntime, err := webadmin.New(webadmin.Config{
		Admin: adminService, Auth: authStore, Sessions: adminSessions,
		LoginGuard: adminauth.NewLoginGuard(time.Now), Logger: logger, Audit: auditQuerier,
	})
	if err != nil {
		return fmt.Errorf("创建 Web 管理服务: %w", err)
	}
	webTLSConfig := tlsBundle.Config.Clone()
	webHTTPServer := &http.Server{
		Handler: webRuntime.Handler(), TLSConfig: webTLSConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	webTLSListener := tls.NewListener(webListener, webTLSConfig)
	keyHandler, err := adminserver.NewKeyHandler(adminService, logger)
	if err != nil {
		return fmt.Errorf("创建 HPAP Key Handler: %w", err)
	}
	runtimeHandler, err := adminserver.NewRuntimeHandler(adminService)
	if err != nil {
		return fmt.Errorf("创建 HPAP Runtime Handler: %w", err)
	}
	connectionHandler, err := adminserver.NewConnectionHandler(adminService)
	if err != nil {
		return fmt.Errorf("创建 HPAP Connection Handler: %w", err)
	}
	sessionHandler, err := adminserver.NewSessionHandler(adminService)
	if err != nil {
		return fmt.Errorf("创建 HPAP Session Handler: %w", err)
	}
	adminMux, err := adminserver.NewMethodMux(keyHandler, runtimeHandler, connectionHandler, sessionHandler)
	if err != nil {
		return fmt.Errorf("创建 HPAP 方法路由: %w", err)
	}
	adminRuntime, err := adminserver.NewServer(adminserver.ServerConfig{Handler: adminMux, Logger: logger})
	if err != nil {
		return fmt.Errorf("创建 HPAP Admin Server: %w", err)
	}

	components := []serverComponent{
		{name: "wecom_connection", run: weComClient.Run},
		{name: "wecom_event_loop", run: func(componentContext context.Context) error {
			return runWeComEventLoop(componentContext, weComClient, router)
		}},
		{name: "relay_http", run: func(context.Context) error { return relayHTTPServer.Serve(relayTLSListener) }},
		{name: "admin", run: func(componentContext context.Context) error {
			return adminRuntime.Serve(componentContext, adminListener)
		}},
		{name: "web_admin", run: func(context.Context) error { return webHTTPServer.Serve(webTLSListener) }},
	}
	shutdown := func(shutdownContext context.Context) {
		_ = adminListener.Close()
		webShutdownContext, cancelWebShutdown := context.WithTimeout(shutdownContext, 5*time.Second)
		_ = webHTTPServer.Shutdown(webShutdownContext)
		cancelWebShutdown()
		hub.BeginShutdown()
		_ = relayHTTPServer.Shutdown(shutdownContext)
		hub.DisconnectAll("server shutdown")
		if err := hub.Wait(shutdownContext); err != nil && logger != nil {
			logger.Warn("等待 HPRP 连接退出超时", "error_type", "shutdown_timeout")
		}
	}
	logger.Info("Herdr Pal Server 启动",
		"bot_hash", shortHash(loaded.WeCom.BotID),
		"listen", listener.Addr().String(),
		"admin_socket", loaded.Server.AdminSocketPath,
		"web_admin_listen", webListener.Addr().String(),
		"verbose", options.Verbose,
		"rate_limit_per_second", loaded.RateLimit.PerSecond,
		"rate_limit_per_minute", loaded.RateLimit.PerMinute,
		"audit_type", loaded.Audit.Type,
		"audit_stderr", loaded.Audit.Stderr,
	)
	return runServerComponents(ctx, stopRequested, components, shutdown, logger)
}

func buildRelayURLHint(addrHint, listen string) (string, error) {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return "", fmt.Errorf("listen 无法提取端口")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("listen 端口无效")
	}

	addrHint = strings.TrimSpace(addrHint)
	if addrHint == "" {
		return fmt.Sprintf("wss://管理员提供的地址:%d", portNumber), nil
	}
	if ip := net.ParseIP(strings.Trim(addrHint, "[]")); ip != nil {
		return "wss://" + net.JoinHostPort(ip.String(), port), nil
	}
	if !validAddressHint(addrHint) {
		return "", fmt.Errorf("addr_hint 必须是主机名或 IP，不能包含协议、端口或路径")
	}
	return "wss://" + net.JoinHostPort(addrHint, port), nil
}

func validAddressHint(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
				continue
			}
			return false
		}
	}
	return true
}

type weComRuntime interface {
	Run(context.Context) error
	Events() <-chan im.IncomingText
}

type serverComponentResult struct {
	component string
	err       error
}

type serverComponent struct {
	name string
	run  func(context.Context) error
}

func runWeComEventLoop(ctx context.Context, weCom weComRuntime, router *server.ConversationRouter) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message, ok := <-weCom.Events():
			if !ok {
				return errors.New("企业微信事件流已关闭")
			}
			router.Handle(ctx, message)
		}
	}
}

func runServerComponents(parent context.Context, stopRequested <-chan struct{}, components []serverComponent, shutdown func(context.Context), logger *slog.Logger) error {
	if parent == nil {
		parent = context.Background()
	}
	runContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	if len(components) == 0 {
		return errors.New("服务端没有可运行组件")
	}
	results := make(chan serverComponentResult, len(components))
	for _, component := range components {
		component := component
		go func() {
			if component.run == nil {
				results <- serverComponentResult{component: component.name, err: errors.New("组件运行函数为空")}
				return
			}
			results <- serverComponentResult{component: component.name, err: component.run(runContext)}
		}()
	}

	var first serverComponentResult
	completed := 0
	normalStop := false
	select {
	case <-parent.Done():
		normalStop = true
	case <-stopRequested:
		normalStop = true
	case first = <-results:
		completed = 1
		if parent.Err() != nil {
			normalStop = true
		} else {
			select {
			case <-stopRequested:
				normalStop = true
			default:
			}
		}
	}

	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if shutdown != nil {
		shutdown(shutdownContext)
	}
	cancel()
	for completed < len(components) {
		select {
		case <-results:
			completed++
		case <-shutdownContext.Done():
			return errors.New("服务端组件未能在 10 秒内停止")
		}
	}
	if normalStop {
		if parent.Err() != nil {
			return parent.Err()
		}
		return nil
	}
	if first.err == nil {
		first.err = errors.New("组件未提供错误即退出")
	}
	if logger != nil {
		logger.Error("服务端组件异常退出", "component", first.component, "error_type", "component_exit", "reason", safeRuntimeReason(first.err))
	}
	return fmt.Errorf("%s: %w", first.component, first.err)
}

func safeRuntimeReason(err error) string {
	if err == nil {
		return "组件未提供退出原因"
	}
	reason := strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(err.Error(), "�"))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "组件未提供退出原因"
	}
	if len(reason) > 240 {
		reason = reason[:240] + "…"
	}
	return reason
}

func printAdminBootstrap(writer io.Writer, bootstrap adminauth.Bootstrap) error {
	if writer == nil || !bootstrap.Created {
		return nil
	}
	_, err := fmt.Fprintf(writer,
		"已创建 Herdr Pal Server 初始管理员，请立即登录并修改密码。\n管理员：%s\n初始密码：%s\n自动化 Token：%s\n",
		bootstrap.Username, bootstrap.InitialPassword, bootstrap.AutomationToken,
	)
	return err
}

func newLogger(writer io.Writer, level string, verbose bool, secrets ...string) (*RuntimeLogger, error) {
	var minimum slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		minimum = slog.LevelInfo
	case "debug":
		minimum = slog.LevelDebug
	case "warn":
		minimum = slog.LevelWarn
	case "error":
		minimum = slog.LevelError
	default:
		return nil, fmt.Errorf("无效日志级别")
	}
	runtimeLogger := &RuntimeLogger{baseLevel: minimum}
	if verbose {
		runtimeLogger.level.Set(slog.LevelDebug)
	} else {
		runtimeLogger.level.Set(minimum)
	}
	runtimeLogger.Logger = slog.New(slog.NewTextHandler(
		redactingWriter{destination: writer, secrets: compactSecrets(secrets)},
		&slog.HandlerOptions{Level: &runtimeLogger.level},
	))
	return runtimeLogger, nil
}

type redactingWriter struct {
	destination io.Writer
	secrets     []string
}

func (writer redactingWriter) Write(data []byte) (int, error) {
	redacted := string(data)
	for _, secret := range writer.secrets {
		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}
	if writer.destination == nil {
		return len(data), nil
	}
	_, err := io.WriteString(writer.destination, redacted)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func compactSecrets(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
