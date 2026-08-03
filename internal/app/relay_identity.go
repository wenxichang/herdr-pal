package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
)

// RelayIdentity 是 Launcher、Supervisor 和普通 Relay 共享的 Herdr Socket 运行身份。
type RelayIdentity struct {
	SocketPath        string
	CanonicalEndpoint string
	SocketHash        string
	LockPath          string
}

// ResolveRelayIdentity 从客户端配置和公开 Herdr 发现入口解析稳定 Socket 身份。
func ResolveRelayIdentity(ctx context.Context, options Options) (RelayIdentity, error) {
	if ctx == nil {
		return RelayIdentity{}, newConfigError("context 不能为空")
	}
	if strings.TrimSpace(options.ConfigPath) == "" {
		return RelayIdentity{}, newConfigError("缺少 -config")
	}
	loaded, err := config.LoadClient(options.ConfigPath)
	if err != nil {
		return RelayIdentity{}, newConfigError(err.Error())
	}
	return resolveRelayIdentityFromConfig(ctx, loaded, options)
}

func resolveRelayIdentityFromConfig(ctx context.Context, loaded config.ClientConfig, options Options) (RelayIdentity, error) {
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
		return RelayIdentity{}, fmt.Errorf("解析 Herdr Socket: %w", err)
	}
	canonicalEndpoint, err := canonicalSocketPath(socketPath)
	if err != nil {
		return RelayIdentity{}, fmt.Errorf("规范化 Herdr Socket: %w", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return RelayIdentity{}, fmt.Errorf("获取缓存目录: %w", err)
	}
	socketHash := shortHash(canonicalEndpoint)
	return RelayIdentity{
		SocketPath:        socketPath,
		CanonicalEndpoint: canonicalEndpoint,
		SocketHash:        socketHash,
		LockPath:          filepath.Join(cacheDir, "herdr-pal", socketHash+".lock"),
	}, nil
}
