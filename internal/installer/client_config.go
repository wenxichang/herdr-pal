// Package installer 负责一体化安装包所需的本地配置合并与安全写入。
package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

func mergeClientConfig(existing []byte, relayURL, relayKey string) ([]byte, error) {
	relayURL = strings.TrimSpace(relayURL)
	relayKey = strings.TrimSpace(relayKey)
	endpoint, err := url.Parse(relayURL)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return nil, fmt.Errorf("relay.url 必须是有效的 wss:// 地址")
	}
	if _, err := credential.BearerCredentialID(relayKey); err != nil {
		return nil, fmt.Errorf("relay.key 必须是有效的 HPRP 机器 Key")
	}

	root, err := decodeJSONObject(existing)
	if err != nil {
		return nil, fmt.Errorf("解析客户端配置: %w", err)
	}
	for field := range root {
		switch field {
		case "relay", "herdr", "log":
		default:
			return nil, fmt.Errorf("客户端配置包含未知字段 %q", field)
		}
	}

	relay := make(map[string]json.RawMessage)
	if raw, ok := root["relay"]; ok {
		if err := json.Unmarshal(raw, &relay); err != nil || relay == nil {
			return nil, fmt.Errorf("客户端配置 relay 必须是对象")
		}
	}
	delete(relay, "userid")
	delete(relay, "machine_id")
	encodedURL, _ := json.Marshal(endpoint.String())
	encodedKey, _ := json.Marshal(relayKey)
	relay["url"] = encodedURL
	relay["key"] = encodedKey
	if _, ok := relay["skip_verify"]; !ok {
		relay["skip_verify"] = json.RawMessage("true")
	}
	relayData, err := json.Marshal(relay)
	if err != nil {
		return nil, fmt.Errorf("编码客户端 relay 配置: %w", err)
	}
	root["relay"] = relayData
	if _, ok := root["herdr"]; !ok {
		root["herdr"] = json.RawMessage(`{}`)
	}
	if _, ok := root["log"]; !ok {
		root["log"] = json.RawMessage(`{"level":"info"}`)
	}

	merged, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("编码客户端配置: %w", err)
	}
	return append(merged, '\n'), nil
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return make(map[string]json.RawMessage), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("配置根节点必须是对象")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("配置文件包含多个 JSON 值")
		}
		return nil, fmt.Errorf("配置文件包含尾随内容: %w", err)
	}
	return root, nil
}
