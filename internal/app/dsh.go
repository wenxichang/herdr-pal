package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/dshlegacy"
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
	lock, err := processlock.Acquire(filepath.Join(lockDir, shortHash("dsh:"+loaded.DSH.URL)+".lock"))
	if err != nil {
		return err
	}
	var timedOut <-chan struct{}
	defer func() { finishProcessLock(lock, &runErr, timedOut) }()
	relay, err := relayclient.New(relayclient.Config{URL: loaded.Relay.URL, Key: loaded.Relay.Key, CredentialID: loaded.Relay.CredentialID, SkipVerify: loaded.Relay.SkipVerify, Version: version.String(), Logger: logger})
	if err != nil {
		return err
	}
	client, err := dshlegacy.NewClient(loaded.DSH.URL, loaded.DSH.Token)
	if err != nil {
		return err
	}
	guard, err := policy.NewGuard(strconv.FormatUint(loaded.Relay.CredentialID, 10))
	if err != nil {
		return err
	}
	executor := dshlegacy.NewExecutor(client, relay, guard)
	if err := relay.SetExecutor(executor); err != nil {
		return err
	}
	logger.Info("Herdr Pal dsh 客户端启动", "credential_id", loaded.Relay.CredentialID)
	runErr, timedOut = runRelayComponents(ctx, relay.Run, executor.Run, defaultShutdownTimeout)
	return runErr
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
