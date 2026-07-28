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
	"time"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

var ErrConfig = errors.New("服务端配置错误")

// Options 是 herdr-pal-server 的外部启动选项。
type Options struct {
	ConfigPath string
	Getenv     func(string) string
	Stderr     io.Writer
	Verbose    bool
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
	runtimeLogger, err := newLogger(stderr, loaded.Log.Level, options.Verbose, loaded.WeCom.Secret)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	logger := runtimeLogger.Logger
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
	tlsBundle, err := server.EnsureTLS(server.TLSConfig{CertFile: loaded.Server.CertFile, KeyFile: loaded.Server.KeyFile, StateDir: loaded.Server.StateDir})
	if err != nil {
		return fmt.Errorf("准备 Relay TLS: %w", err)
	}
	weComClient, err := wecom.NewClient(wecom.ClientConfig{
		Endpoint: wecom.DefaultEndpoint, BotID: loaded.WeCom.BotID, Secret: loaded.WeCom.Secret, Logger: logger,
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
		server.ConversationRouterConfig{RelayURL: relayURL},
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
	httpServer := &http.Server{Handler: hub, TLSConfig: tlsBundle.Config, ReadHeaderTimeout: 10 * time.Second}
	tlsListener := tls.NewListener(listener, tlsBundle.Config)
	logger.Info("Herdr Pal Server 启动", "bot_hash", shortHash(loaded.WeCom.BotID), "listen", loaded.Server.Listen, "verbose", options.Verbose)
	return runServerComponents(ctx, weComClient, router, httpServer, tlsListener, logger)
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

func runServerComponents(ctx context.Context, weCom weComRuntime, router *server.ConversationRouter, httpServer *http.Server, listener net.Listener, logger *slog.Logger) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan serverComponentResult, 3)
	go func() { results <- serverComponentResult{component: "wecom_connection", err: weCom.Run(runContext)} }()
	go func() {
		for {
			select {
			case <-runContext.Done():
				results <- serverComponentResult{component: "wecom_event_loop", err: runContext.Err()}
				return
			case message, ok := <-weCom.Events():
				if !ok {
					results <- serverComponentResult{component: "wecom_event_loop", err: errors.New("企业微信事件流已关闭")}
					return
				}
				router.Handle(runContext, message)
			}
		}
	}()
	go func() { results <- serverComponentResult{component: "relay_http", err: httpServer.Serve(listener)} }()
	first := <-results
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownContext)
	for remaining := 2; remaining > 0; remaining-- {
		select {
		case <-results:
		case <-shutdownContext.Done():
			remaining = 0
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(first.err, http.ErrServerClosed) || errors.Is(first.err, context.Canceled) {
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
