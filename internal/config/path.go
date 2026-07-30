package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const defaultConfigDirectory = "herdr-pal"

// DefaultClientPath 返回客户端在用户 HOME 下的默认配置文件路径。
func DefaultClientPath() (string, error) {
	return defaultPath("config.json")
}

// DefaultServerPath 返回服务端在用户 HOME 下的默认配置文件路径。
func DefaultServerPath() (string, error) {
	return defaultPath("server-config.json")
}

// DefaultServerAuthPath 返回 Web 管理员摘要文件的固定路径。
func DefaultServerAuthPath() (string, error) {
	return defaultPath("server-auth.json")
}

func defaultPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户 HOME 目录: %w", err)
	}
	return filepath.Join(home, ".config", defaultConfigDirectory, name), nil
}
