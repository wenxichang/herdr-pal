package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/dsh"
)

var (
	// configWatchInterval 是轮询配置文件的周期。
	//
	// 使用轮询而不是文件系统事件，避免引入额外依赖和 inotify 在容器/网络文件
	// 系统上的跨平台差异。声明为变量以便测试缩短等待。
	configWatchInterval = 5 * time.Second
	// configMaxBytes 限制读取的配置文件大小，避免异常文件耗尽内存。
	configMaxBytes int64 = 1 << 20
)

// dshConfigUpdater 是配置热更新的接收方。
// 生产实现是 *dsh.Backend，测试可注入 fake。
type dshConfigUpdater interface {
	UpdateDiscoveryConfig(dsh.DiscoveryConfig) (bool, error)
}

// watchDSHConfig 每 configWatchInterval 检查一次配置文件，内容（MD5）变化时
// 重新加载并热替换 dsh 发现配置，直到 ctx 结束。
//
// 编辑器通常先截断再写入，因此中途读到的是半截 JSON；此时不报错退出，只在
// 下一轮重试，只有内容稳定且成功解码后才应用。读取失败同样只记录日志重试，
// 不影响正在运行的连接。relay 等其他配置项只在进程启动时生效。
func watchDSHConfig(ctx context.Context, path string, updater dshConfigUpdater, logger *slog.Logger) error {
	if ctx == nil {
		return errors.New("配置监听缺少 context")
	}
	if updater == nil {
		return errors.New("配置监听缺少 dsh 配置接收方")
	}
	if logger == nil {
		logger = slog.Default()
	}

	lastDigest, err := configDigest(path)
	if err != nil {
		// 启动时刚刚成功加载过配置，这里失败只说明读不到了；继续监听。
		logger.Warn("读取 dsh 配置文件失败，等待下次变更", "path", path, "error", err)
		lastDigest = ""
	}

	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		digest, err := configDigest(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("读取 dsh 配置文件失败，等待下次变更", "path", path, "error", err)
			}
			continue
		}
		if digest == lastDigest {
			continue
		}
		loaded, err := config.LoadClient(path)
		if err != nil {
			// 编辑器通常先截断再写入，可能读到半截 JSON；此处不推进摘要，
			// 下一轮内容稳定后会重新解码成功。若文件一直是坏的，每轮都会
			// 记录一次 warn，便于发现真正的配置错误。
			logger.Warn("dsh 配置文件当前无效，等待下次轮询", "path", path, "error", err)
			continue
		}
		lastDigest = digest
		if loaded.DSH == nil {
			logger.Warn("dsh 配置已移除，保留当前配置", "path", path)
			continue
		}
		changed, err := updater.UpdateDiscoveryConfig(dsh.DiscoveryConfig{
			URL:                loaded.DSH.URL,
			Home:               loaded.DSH.Home,
			Variant:            loaded.DSH.Variant,
			IncludeUnavailable: loaded.DSH.IncludeUnavailable,
		})
		if err != nil {
			logger.Warn("dsh 配置热更新失败，保留当前配置", "path", path, "error", err)
			continue
		}
		if !changed {
			continue
		}
		logger.Info("dsh 配置已热更新",
			"path", path,
			"dsh_url", loaded.DSH.URL,
			"include_unavailable", loaded.DSH.IncludeUnavailable,
		)
	}
}

// configDigest 返回文件内容的 MD5 十六进制摘要，用于判断是否发生变化。
func configDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开配置文件: %w", err)
	}
	defer file.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, io.LimitReader(file, configMaxBytes+1)); err != nil {
		return "", fmt.Errorf("读取配置文件: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
