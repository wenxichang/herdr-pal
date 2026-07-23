package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsConfigurationAndSecretFromEnvironment(t *testing.T) {
	path := writeConfig(t, `{
  "wecom": {"bot_id": "bot-1", "allowed_user_id": "user-1"},
  "herdr": {"session": "session-1", "socket_path": "/tmp/herdr.sock"},
  "log": {"level": "debug"}
}`)

	config, err := Load(path, func(name string) string {
		if name != SecretEnvName {
			t.Fatalf("getenv 收到意外变量名：%s", name)
		}
		return "secret-value"
	})
	if err != nil {
		t.Fatalf("Load() 返回错误：%v", err)
	}

	if config.WeCom.BotID != "bot-1" || config.WeCom.AllowedUserID != "user-1" || config.WeCom.Secret != "secret-value" {
		t.Fatalf("WeCom 配置不正确：%+v", config.WeCom)
	}
	if config.Herdr.Session != "session-1" || config.Herdr.SocketPath != "/tmp/herdr.sock" {
		t.Fatalf("Herdr 配置不正确：%+v", config.Herdr)
	}
	if config.Log.Level != "debug" {
		t.Fatalf("Log 配置不正确：%+v", config.Log)
	}
}

func TestLoadRejectsMissingRequiredValuesWithoutLeakingSecret(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		secret string
		field  string
	}{
		{name: "空白 bot_id", json: `{"wecom":{"bot_id":" \t ","allowed_user_id":"user"},"herdr":{},"log":{}}`, secret: "secret-value", field: "bot_id"},
		{name: "缺少 allowed_user_id", json: `{"wecom":{"bot_id":"bot"},"herdr":{},"log":{}}`, secret: "secret-value", field: "allowed_user_id"},
		{name: "空白 Secret", json: `{"wecom":{"bot_id":"bot","allowed_user_id":"user"},"herdr":{},"log":{}}`, secret: " \n ", field: "secret"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.json), func(string) string { return test.secret })
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("Load() 错误 = %v，期望包含字段名 %q", err, test.field)
			}
			if strings.Contains(err.Error(), test.secret) {
				t.Fatalf("Load() 错误泄露 Secret：%v", err)
			}
		})
	}
}

func TestLoadRejectsUnknownAndTrailingJSON(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "未知字段", json: `{"wecom":{"bot_id":"bot","allowed_user_id":"user","unexpected":true},"herdr":{},"log":{}}`},
		{name: "第二个 JSON", json: `{"wecom":{"bot_id":"bot","allowed_user_id":"user"},"herdr":{},"log":{}} {}`},
		{name: "尾随内容", json: `{"wecom":{"bot_id":"bot","allowed_user_id":"user"},"herdr":{},"log":{}} trailing`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.json), func(string) string { return "secret-value" })
			if err == nil {
				t.Fatal("Load() 未拒绝非法 JSON")
			}
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败：%v", err)
	}
	return path
}
