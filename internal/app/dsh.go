package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/dsh"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func runDSHRelay(ctx context.Context, loaded config.ClientConfig, logger *slog.Logger) (runErr error) {
	if loaded.DSH == nil {
		return newConfigError("缺少 dsh 配置")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(cacheDir, "herdr-pal")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return err
	}
	lock, err := processlock.Acquire(filepath.Join(lockDir, dshLockName(*loaded.DSH)+".lock"))
	if err != nil {
		return err
	}
	var timedOut <-chan struct{}
	defer func() { finishProcessLock(lock, &runErr, timedOut) }()
	relay, err := relayclient.New(relayclient.Config{URL: loaded.Relay.URL, Key: loaded.Relay.Key, CredentialID: loaded.Relay.CredentialID, SkipVerify: loaded.Relay.SkipVerify, Version: version.String(), Logger: logger})
	if err != nil {
		return err
	}
	guard, err := policy.NewGuard(strconv.FormatUint(loaded.Relay.CredentialID, 10))
	if err != nil {
		return err
	}
	backend, err := newDSHBackend(*loaded.DSH, relay, guard, logger)
	if err != nil {
		return err
	}
	if err := relay.SetExecutor(backend); err != nil {
		return err
	}
	logger.Info("Herdr Pal dsh 后端启动",
		"credential_id", loaded.Relay.CredentialID,
		"dsh_variant", dshVariant(loaded.DSH.Variant),
		"discovery", dshDiscoveryMode(loaded.DSH.URL),
	)
	runErr, timedOut = runRelayComponents(ctx, relay.Run, backend.Run, defaultShutdownTimeout)
	return runErr
}

// dshLockName 返回 dsh 后端的进程锁文件名。
//
// 自动发现只能运行一个实例，因此共享固定身份；显式 URL 按地址区分，
// 允许同时连接不同的 dsh 宿主。
func dshLockName(cfg config.DSHConfig) string {
	key := "dsh:auto"
	if cfg.URL != "" {
		key = "dsh:" + cfg.URL
	}
	return shortHash(key)
}

func newDSHBackend(cfg config.DSHConfig, sink dsh.Sink, guard *policy.Guard, logger *slog.Logger) (*dsh.Backend, error) {
	return dsh.NewBackend(dsh.DiscoveryConfig{URL: cfg.URL, Home: cfg.Home, Variant: cfg.Variant}, sink, guard, logger)
}

func dshVariant(value string) string {
	if value == "" || value == "auto" {
		return "auto"
	}
	return value
}

// dshVariantForConfig 返回日志使用的 dsh 变体；未启用 dsh 时为空。
func dshVariantForConfig(cfg *config.DSHConfig) string {
	if cfg == nil {
		return ""
	}
	return dshVariant(cfg.Variant)
}

func dshDiscoveryMode(url string) string {
	if url == "" {
		return "auto"
	}
	return "explicit"
}

func runBackendPair(ctx context.Context, first, second func(context.Context) error) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- first(child) }()
	go func() { results <- second(child) }()
	err := <-results
	cancel()
	<-results
	return err
}
