package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServerConfig 是 herdr-pal-server 的完整配置。
type ServerConfig struct {
	WeCom  ServerWeComConfig `json:"wecom"`
	Server ListenerConfig    `json:"server"`
	Log    LogConfig         `json:"log"`
}

// ServerWeComConfig 是服务端独占的企业微信机器人配置。
type ServerWeComConfig struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"-"`
}

// ListenerConfig 是 Relay WSS 监听和证书配置。
type ListenerConfig struct {
	Listen   string `json:"listen"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	StateDir string `json:"state_dir"`
}

// LoadServer 加载服务端配置并从环境读取企业微信 Secret。
func LoadServer(path string, getenv func(string) string) (ServerConfig, error) {
	loaded, err := decodeFile[ServerConfig](path)
	if err != nil {
		return ServerConfig{}, err
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	loaded.WeCom.Secret = getenv(SecretEnvName)
	if strings.TrimSpace(loaded.WeCom.BotID) == "" {
		return ServerConfig{}, fmt.Errorf("缺少必填字段 bot_id")
	}
	if strings.TrimSpace(loaded.WeCom.Secret) == "" {
		return ServerConfig{}, fmt.Errorf("缺少环境变量 %s", SecretEnvName)
	}
	if strings.TrimSpace(loaded.Server.Listen) == "" {
		return ServerConfig{}, fmt.Errorf("缺少必填字段 listen")
	}
	certConfigured := strings.TrimSpace(loaded.Server.CertFile) != ""
	keyConfigured := strings.TrimSpace(loaded.Server.KeyFile) != ""
	if certConfigured != keyConfigured {
		return ServerConfig{}, fmt.Errorf("cert_file 与 key_file 必须同时配置")
	}
	if strings.TrimSpace(loaded.Server.StateDir) == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return ServerConfig{}, fmt.Errorf("获取用户配置目录: %w", err)
		}
		loaded.Server.StateDir = filepath.Join(configDir, "herdr-pal-server")
	}
	if strings.TrimSpace(loaded.Log.Level) == "" {
		loaded.Log.Level = "info"
	}
	return loaded, nil
}
