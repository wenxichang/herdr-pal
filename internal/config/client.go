package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

// ClientConfig 是 herdr-pal 网络模式的完整配置。
type ClientConfig struct {
	Relay RelayConfig `json:"relay"`
	Herdr HerdrConfig `json:"herdr"`
	Log   LogConfig   `json:"log"`
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

// LoadClient 加载并校验 herdr-pal HPRP 网络模式配置。
func LoadClient(path string) (ClientConfig, error) {
	loaded, err := decodeFile[ClientConfig](path)
	if err != nil {
		return ClientConfig{}, err
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
