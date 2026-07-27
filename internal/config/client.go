package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

// ClientConfig 是 herdr-pal 网络模式的完整配置。
type ClientConfig struct {
	Relay RelayConfig `json:"relay"`
	Herdr HerdrConfig `json:"herdr"`
	Log   LogConfig   `json:"log"`
}

// RelayConfig 是客户端连接中央 Relay Server 的配置。
type RelayConfig struct {
	URL        string `json:"url"`
	UserID     string `json:"userid"`
	MachineID  string `json:"machine_id"`
	SkipVerify bool   `json:"-"`
}

// UnmarshalJSON 在保持严格字段校验的同时区分未配置和显式 false。
func (config *RelayConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		URL        string `json:"url"`
		UserID     string `json:"userid"`
		MachineID  string `json:"machine_id"`
		SkipVerify *bool  `json:"skip_verify"`
	}
	if err := decodeStrict(bytes.NewReader(data), &raw); err != nil {
		return err
	}
	config.URL = raw.URL
	config.UserID = raw.UserID
	config.MachineID = raw.MachineID
	config.SkipVerify = raw.SkipVerify == nil || *raw.SkipVerify
	return nil
}

// MarshalJSON 保留面向工具的完整配置表示。
func (config RelayConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		URL        string `json:"url"`
		UserID     string `json:"userid"`
		MachineID  string `json:"machine_id"`
		SkipVerify bool   `json:"skip_verify"`
	}{config.URL, config.UserID, config.MachineID, config.SkipVerify})
}

// LoadClient 加载并校验 herdr-pal Relay 网络模式配置，machine_id 留空时使用系统 hostname。
func LoadClient(path string) (ClientConfig, error) {
	return loadClient(path, os.Hostname)
}

func loadClient(path string, hostname func() (string, error)) (ClientConfig, error) {
	loaded, err := decodeFile[ClientConfig](path)
	if err != nil {
		return ClientConfig{}, err
	}
	endpoint, err := url.Parse(strings.TrimSpace(loaded.Relay.URL))
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return ClientConfig{}, fmt.Errorf("relay.url 必须是有效的 wss:// 地址")
	}
	if strings.TrimSpace(loaded.Relay.MachineID) == "" {
		machineID, err := hostname()
		if err != nil {
			return ClientConfig{}, fmt.Errorf("获取系统 hostname: %w", err)
		}
		loaded.Relay.MachineID = strings.TrimSpace(machineID)
		if loaded.Relay.MachineID == "" {
			return ClientConfig{}, fmt.Errorf("系统 hostname 为空，请配置 relay.machine_id")
		}
	}
	if err := relayproto.ValidateClientHello(relayproto.ClientHello{
		UserID: loaded.Relay.UserID, MachineID: loaded.Relay.MachineID,
	}); err != nil {
		return ClientConfig{}, fmt.Errorf("relay 身份配置无效: %w", err)
	}
	loaded.Relay.URL = endpoint.String()
	if strings.TrimSpace(loaded.Log.Level) == "" {
		loaded.Log.Level = "info"
	}
	return loaded, nil
}
