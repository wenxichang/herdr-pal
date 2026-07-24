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
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/config"
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
	logger, err := newLogger(stderr, loaded.Log.Level)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
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
	tlsConfig, err := server.EnsureTLS(server.TLSConfig{CertFile: loaded.Server.CertFile, KeyFile: loaded.Server.KeyFile, StateDir: loaded.Server.StateDir})
	if err != nil {
		return fmt.Errorf("准备 Relay TLS: %w", err)
	}
	weComClient, err := wecom.NewClient(wecom.ClientConfig{
		Endpoint: wecom.DefaultEndpoint, BotID: loaded.WeCom.BotID, Secret: loaded.WeCom.Secret, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("创建企业微信客户端: %w", err)
	}
	catalog := server.NewSessionCatalog()
	hub, err := server.NewClientHub(catalog, server.HubConfig{}, logger)
	if err != nil {
		return err
	}
	deduper, err := policy.NewDeduper(24*time.Hour, 10000, time.Now)
	if err != nil {
		return err
	}
	router, err := server.NewConversationRouter(catalog, server.NewUserExecutor(64), weComClient, hub, deduper, logger)
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
	httpServer := &http.Server{Handler: hub, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second}
	tlsListener := tls.NewListener(listener, tlsConfig)
	logger.Info("Herdr Pal Server 启动", "bot_hash", shortHash(loaded.WeCom.BotID), "listen", loaded.Server.Listen)
	return runServerComponents(ctx, weComClient, router, httpServer, tlsListener)
}

type weComRuntime interface {
	Run(context.Context) error
	Events() <-chan im.IncomingText
}

func runServerComponents(ctx context.Context, weCom weComRuntime, router *server.ConversationRouter, httpServer *http.Server, listener net.Listener) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 3)
	go func() { results <- weCom.Run(runContext) }()
	go func() {
		for {
			select {
			case <-runContext.Done():
				results <- runContext.Err()
				return
			case message, ok := <-weCom.Events():
				if !ok {
					results <- errors.New("企业微信事件流已关闭")
					return
				}
				router.Handle(runContext, message)
			}
		}
	}()
	go func() { results <- httpServer.Serve(listener) }()
	first := <-results
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownContext)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(first, http.ErrServerClosed) || errors.Is(first, context.Canceled) {
		return nil
	}
	return first
}

func newLogger(writer io.Writer, level string) (*slog.Logger, error) {
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
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: minimum})), nil
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
