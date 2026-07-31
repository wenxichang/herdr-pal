package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHerdrConfigPath 返回 Herdr 在 Linux/macOS 上遵循 XDG 的默认配置路径。
func DefaultHerdrConfigPath(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if xdgConfigHome := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdgConfigHome != "" {
		return filepath.Join(xdgConfigHome, "herdr", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户 HOME 目录: %w", err)
	}
	return filepath.Join(home, ".config", "herdr", "config.toml"), nil
}
