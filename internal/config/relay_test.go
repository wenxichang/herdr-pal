package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadClientDefaultsSkipVerifyAndAcceptsExplicitFalse(t *testing.T) {
	defaultPath := writeConfig(t, `{
  "relay": {"url": "wss://relay.internal:9443", "userid": "user-1", "machine_id": "home-mac"},
  "herdr": {},
  "log": {}
}`)
	loaded, err := LoadClient(defaultPath)
	if err != nil {
		t.Fatalf("LoadClient() error = %v", err)
	}
	if !loaded.Relay.SkipVerify || loaded.Log.Level != "info" {
		t.Fatalf("LoadClient() = %#v", loaded)
	}

	strictPath := writeConfig(t, `{
  "relay": {"url": "wss://relay.internal:9443", "userid": "user-1", "machine_id": "home-mac", "skip_verify": false},
  "herdr": {},
  "log": {"level": "debug"}
}`)
	loaded, err = LoadClient(strictPath)
	if err != nil {
		t.Fatalf("LoadClient(explicit false) error = %v", err)
	}
	if loaded.Relay.SkipVerify || loaded.Log.Level != "debug" {
		t.Fatalf("LoadClient(explicit false) = %#v", loaded)
	}
}

func TestLoadClientRejectsPlainWSOldWeComAndUnknownNestedField(t *testing.T) {
	tests := []string{
		`{"relay":{"url":"ws://relay:9443","userid":"user","machine_id":"machine"},"herdr":{},"log":{}}`,
		`{"wecom":{"bot_id":"old"},"relay":{"url":"wss://relay:9443","userid":"user","machine_id":"machine"},"herdr":{},"log":{}}`,
		`{"relay":{"url":"wss://relay:9443","userid":"user","machine_id":"machine","unknown":true},"herdr":{},"log":{}}`,
	}
	for _, raw := range tests {
		if _, err := LoadClient(writeConfig(t, raw)); err == nil {
			t.Fatalf("LoadClient() accepted %s", raw)
		}
	}
}

func TestLoadServerReadsSecretAndDefaultsStateDirectory(t *testing.T) {
	path := writeConfig(t, `{
  "wecom": {"bot_id": "bot-1"},
  "server": {"listen": "127.0.0.1:9443"},
  "log": {}
}`)
	loaded, err := LoadServer(path, func(name string) string {
		if name != SecretEnvName {
			t.Fatalf("getenv name = %q", name)
		}
		return "secret-value"
	})
	if err != nil {
		t.Fatalf("LoadServer() error = %v", err)
	}
	if loaded.WeCom.BotID != "bot-1" || loaded.WeCom.Secret != "secret-value" || loaded.Server.Listen != "127.0.0.1:9443" {
		t.Fatalf("LoadServer() = %#v", loaded)
	}
	if filepath.Base(loaded.Server.StateDir) != "herdr-pal-server" || loaded.Log.Level != "info" {
		t.Fatalf("LoadServer() defaults = %#v", loaded)
	}
}

func TestLoadServerRequiresListenSecretAndCertificatePair(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		secret string
		field  string
	}{
		{name: "listen", raw: `{"wecom":{"bot_id":"bot"},"server":{},"log":{}}`, secret: "secret", field: "listen"},
		{name: "secret", raw: `{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443"},"log":{}}`, secret: " ", field: SecretEnvName},
		{name: "certificate pair", raw: `{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443","cert_file":"cert.pem"},"log":{}}`, secret: "secret", field: "cert_file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadServer(writeConfig(t, test.raw), func(string) string { return test.secret })
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("LoadServer() error = %v, want field %q", err, test.field)
			}
			if strings.TrimSpace(test.secret) != "" && strings.Contains(err.Error(), test.secret) {
				t.Fatalf("LoadServer() leaked secret: %v", err)
			}
		})
	}
}
