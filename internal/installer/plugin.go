package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const pluginLinkOutputLimit = 8 * 1024

type pluginArtifacts struct {
	Manifest []byte
	Launcher []byte
}

// DefaultPluginDirectory 返回客户端配置旁的 Herdr Pal startup 插件目录。
func DefaultPluginDirectory(clientConfigPath string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(clientConfigPath)), "plugin")
}

func buildPluginArtifacts(palBinaryPath, clientConfigPath, pluginVersion string) (pluginArtifacts, error) {
	palBinaryPath = filepath.Clean(strings.TrimSpace(palBinaryPath))
	clientConfigPath = filepath.Clean(strings.TrimSpace(clientConfigPath))
	if !filepath.IsAbs(palBinaryPath) || !filepath.IsAbs(clientConfigPath) {
		return pluginArtifacts{}, errors.New("Pal 插件路径必须使用绝对路径")
	}
	pluginVersion = strings.TrimPrefix(strings.TrimSpace(pluginVersion), "v")
	if pluginVersion == "" {
		pluginVersion = "dev"
	}
	manifest := fmt.Sprintf(`id = "herdr-pal.autostart"
name = "Herdr Pal Autostart"
version = %s
min_herdr_version = "0.7.5"
description = "Starts and supervises Herdr Pal after the public Herdr API is ready."
platforms = ["linux", "macos"]

[[startup]]
command = ["./start-herdr-pal"]
`, strconv.Quote(pluginVersion))
	launcher := "#!/bin/sh\nset -eu\nexec " + shellSingleQuote(palBinaryPath) + " start -config " + shellSingleQuote(clientConfigPath) + "\n"
	return pluginArtifacts{Manifest: []byte(manifest), Launcher: []byte(launcher)}, nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func writePluginDirectory(path string, artifacts pluginArtifacts, now time.Time) (backupPath string, returnErr error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) || len(artifacts.Manifest) == 0 || len(artifacts.Launcher) == 0 {
		return "", errors.New("Pal 插件目录或内容无效")
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent); err != nil {
		return "", fmt.Errorf("准备 Pal 插件父目录: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("检查 Pal 插件目录: %w", err)
	}
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("Pal 插件路径不是普通目录")
		}
		backupPath, err = allocatePluginBackupPath(path, now)
		if err != nil {
			return "", err
		}
		if err := os.Rename(path, backupPath); err != nil {
			return "", fmt.Errorf("备份 Pal 插件目录: %w", err)
		}
	}

	temporary, err := os.MkdirTemp(parent, ".herdr-pal-plugin-*")
	if err != nil {
		_ = restorePluginDirectory(path, backupPath)
		return "", fmt.Errorf("创建 Pal 插件临时目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = restorePluginDirectory(path, backupPath)
		return "", fmt.Errorf("设置 Pal 插件临时目录权限: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "herdr-plugin.toml"), artifacts.Manifest, 0o600); err != nil {
		_ = restorePluginDirectory(path, backupPath)
		return "", fmt.Errorf("写入 Pal 插件清单: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "start-herdr-pal"), artifacts.Launcher, 0o700); err != nil {
		_ = restorePluginDirectory(path, backupPath)
		return "", fmt.Errorf("写入 Pal 插件启动器: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = restorePluginDirectory(path, backupPath)
		return "", fmt.Errorf("安装 Pal 插件目录: %w", err)
	}
	return backupPath, nil
}

func allocatePluginBackupPath(path string, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	base := fmt.Sprintf("%s.bak-%s-%d", path, now.UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	for index := 0; index < 100; index++ {
		candidate := base
		if index > 0 {
			candidate = fmt.Sprintf("%s-%d", base, index)
		}
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("检查 Pal 插件备份路径: %w", err)
		}
	}
	return "", errors.New("无法分配 Pal 插件备份目录")
}

func restorePluginDirectory(path, backupPath string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("无法清理非普通 Pal 插件目录")
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("移除新 Pal 插件目录: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查新 Pal 插件目录: %w", err)
	}
	if backupPath == "" {
		return nil
	}
	if err := os.Rename(backupPath, path); err != nil {
		return fmt.Errorf("恢复 Pal 插件目录: %w", err)
	}
	return nil
}

func runHerdrPluginLink(ctx context.Context, herdrBinaryPath, herdrConfigPath, pluginDirectory string) error {
	command := exec.CommandContext(ctx, herdrBinaryPath, "plugin", "link", pluginDirectory)
	command.Env = replaceEnvironment(os.Environ(), "HERDR_CONFIG_PATH", herdrConfigPath)
	var output limitedBuffer
	output.limit = pluginLinkOutputLimit
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := strings.TrimSpace(output.String())
		if reason == "" {
			return fmt.Errorf("执行 herdr plugin link: %w", err)
		}
		return fmt.Errorf("执行 herdr plugin link: %w: %s", err, reason)
	}
	return nil
}
