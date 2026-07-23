// Package config 负责加载并校验 Herdr Pal 的本地配置。
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// SecretEnvName 是企业微信机器人密钥使用的环境变量名称。
const SecretEnvName = "HERDR_PAL_WECOM_SECRET"

// Config 是 Herdr Pal 的完整配置。
type Config struct {
	WeCom WeComConfig `json:"wecom"`
	Herdr HerdrConfig `json:"herdr"`
	Log   LogConfig   `json:"log"`
}

// WeComConfig 是企业微信智能机器人配置。
type WeComConfig struct {
	BotID         string `json:"bot_id"`
	AllowedUserID string `json:"allowed_user_id"`
	Secret        string `json:"-"`
}

// HerdrConfig 是 Herdr 本地 Socket 连接配置。
type HerdrConfig struct {
	Session    string `json:"session"`
	SocketPath string `json:"socket_path"`
}

// LogConfig 是日志配置。
type LogConfig struct {
	Level string `json:"level"`
}

// Load 从 path 读取严格的 JSON 配置，并从 getenv 获取企业微信机器人密钥。
func Load(path string, getenv func(string) string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()

	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("解析配置文件: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("配置文件包含多个 JSON 值")
		}
		return Config{}, fmt.Errorf("配置文件包含尾随内容: %w", err)
	}

	config.WeCom.Secret = getenv(SecretEnvName)
	if strings.TrimSpace(config.WeCom.BotID) == "" {
		return Config{}, fmt.Errorf("缺少必填字段 bot_id")
	}
	if strings.TrimSpace(config.WeCom.AllowedUserID) == "" {
		return Config{}, fmt.Errorf("缺少必填字段 allowed_user_id")
	}
	if strings.TrimSpace(config.WeCom.Secret) == "" {
		return Config{}, fmt.Errorf("缺少必填字段 secret")
	}

	return config, nil
}
