package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultClientConfigDirectory = "herdr-pal"
	defaultServerConfigDirectory = "herdr-pal-server"
)

// DefaultClientPath 返回客户端在用户 HOME 下的默认配置文件路径。
func DefaultClientPath() (string, error) {
	return defaultPath(defaultClientConfigDirectory, "config.json")
}

// DefaultServerPath 返回服务端在用户 HOME 下的默认配置文件路径。
func DefaultServerPath() (string, error) {
	return defaultPath(defaultServerConfigDirectory, "server.json")
}

// DefaultServerAuthPath 返回 Web 管理员摘要文件的固定路径。
func DefaultServerAuthPath() (string, error) {
	return defaultPath(defaultServerConfigDirectory, "auth.json")
}

// DefaultServerBootstrapPath 返回首次管理员引导文件的固定路径。
func DefaultServerBootstrapPath() (string, error) {
	return defaultPath(defaultServerConfigDirectory, "bootstrap.txt")
}

// DefaultServerHelpPath 返回企业微信实时帮助文件的固定路径。
func DefaultServerHelpPath() (string, error) {
	return defaultPath(defaultServerConfigDirectory, "help.md")
}

func defaultPath(directory, name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户 HOME 目录: %w", err)
	}
	return filepath.Join(home, ".config", directory, name), nil
}
