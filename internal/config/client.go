package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

// ClientConfig 是 herdr-pal 网络模式的完整配置。
type ClientConfig struct {
	Relay RelayConfig `json:"relay"`
	Herdr HerdrConfig `json:"herdr"`
	Log   LogConfig   `json:"log"`
	DSH   *DSHConfig  `json:"dsh,omitempty"`
}

// DSHConfig 是本机 dsh 后端配置；出现该节点即启用 dsh 后端。
type DSHConfig struct {
	URL     string `json:"url,omitempty"`
	Home    string `json:"home,omitempty"`
	Variant string `json:"variant,omitempty"`
}

// UnmarshalJSON 严格读取 dsh 原生字段，并对已移除的旧插件 token 返回迁移提示。
func (config *DSHConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		URL     string `json:"url"`
		Home    string `json:"home"`
		Variant string `json:"variant"`
		Token   string `json:"token"`
	}
	if err := decodeStrict(bytes.NewReader(data), &raw); err != nil {
		return err
	}
	if raw.Token != "" {
		return errors.New("dsh.token 已移除：Pal 现在直接连接 dsh Remote API")
	}
	config.URL = raw.URL
	config.Home = raw.Home
	config.Variant = raw.Variant
	return nil
}

// RelayConfig 是客户端连接中央 Relay Server 的配置。
type RelayConfig struct {
	URL          string `json:"url"`
	Key          string `json:"key"`
	SkipVerify   bool   `json:"-"`
	CredentialID uint64 `json:"-"`
}

// UnmarshalJSON 在保持严格字段校验的同时区分未配置和显式 false。
func (config *RelayConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		URL        string `json:"url"`
		Key        string `json:"key"`
		SkipVerify *bool  `json:"skip_verify"`
	}
	if err := decodeStrict(bytes.NewReader(data), &raw); err != nil {
		return err
	}
	config.URL = raw.URL
	config.Key = raw.Key
	config.SkipVerify = raw.SkipVerify == nil || *raw.SkipVerify
	return nil
}

// MarshalJSON 保留面向工具的完整配置表示。
func (config RelayConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		URL        string `json:"url"`
		Key        string `json:"key"`
		SkipVerify bool   `json:"skip_verify"`
	}{config.URL, config.Key, config.SkipVerify})
}

// validateDSHConfig 校验原生 dsh 后端配置。
func validateDSHConfig(cfg DSHConfig) error {
	if cfg.URL != "" {
		endpoint, err := url.Parse(cfg.URL)
		if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" ||
			endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return errors.New("dsh.url 必须是 http://127.0.0.1:端口，不能包含路径或凭据")
		}
	}
	if cfg.Home != "" && !filepath.IsAbs(cfg.Home) {
		return errors.New("dsh.home 必须是绝对路径")
	}
	switch cfg.Variant {
	case "", "auto", "web", "desktop":
	default:
		return errors.New("dsh.variant 只能是 auto、web 或 desktop")
	}
	return nil
}

// LoadClient 加载并校验 herdr-pal HPRP 网络模式配置。
func LoadClient(path string) (ClientConfig, error) {
	loaded, err := decodeFile[ClientConfig](path)
	if err != nil {
		return ClientConfig{}, err
	}
	if loaded.DSH != nil {
		if err := validateDSHConfig(*loaded.DSH); err != nil {
			return ClientConfig{}, err
		}
	}
	if loaded.Herdr.Disabled && loaded.DSH == nil {
		return ClientConfig{}, fmt.Errorf("禁用 Herdr 时必须配置 dsh")
	}
	endpoint, err := url.Parse(strings.TrimSpace(loaded.Relay.URL))
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return ClientConfig{}, fmt.Errorf("relay.url 必须是有效的 wss:// 地址")
	}
	credentialID, err := credential.BearerCredentialID(strings.TrimSpace(loaded.Relay.Key))
	if err != nil {
		return ClientConfig{}, fmt.Errorf("relay.key 必须是有效的 HPRP 机器 Key")
	}
	loaded.Relay.Key = strings.TrimSpace(loaded.Relay.Key)
	loaded.Relay.CredentialID = credentialID
	loaded.Relay.URL = endpoint.String()
	if strings.TrimSpace(loaded.Log.Level) == "" {
		loaded.Log.Level = "info"
	}
	return loaded, nil
}
