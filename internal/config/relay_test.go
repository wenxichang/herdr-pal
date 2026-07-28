package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

func TestLoadClientDefaultsSkipVerifyAndAcceptsExplicitFalse(t *testing.T) {
	key := testRelayKey(t)
	defaultPath := writeConfig(t, `{
  "relay": {"url": "wss://relay.internal:9443", "key": "`+key+`"},
  "herdr": {},
  "log": {}
}`)
	loaded, err := LoadClient(defaultPath)
	if err != nil {
		t.Fatalf("LoadClient() error = %v", err)
	}
	if !loaded.Relay.SkipVerify || loaded.Log.Level != "info" || loaded.Relay.Key != key {
		t.Fatalf("LoadClient() = %#v", loaded)
	}

	strictPath := writeConfig(t, `{
  "relay": {"url": "wss://relay.internal:9443", "key": "`+key+`", "skip_verify": false},
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

func TestLoadClientRejectsMissingOrMalformedKey(t *testing.T) {
	for _, key := range []string{"", "plain-secret", "hpk_bad"} {
		path := writeConfig(t, `{
  "relay": {"url": "wss://relay.internal:9443", "key": "`+key+`"},
  "herdr": {},
  "log": {}
}`)
		if _, err := LoadClient(path); err == nil || !strings.Contains(err.Error(), "relay.key") {
			t.Fatalf("LoadClient(%q) error = %v, want relay.key", key, err)
		}
	}
}

func TestLoadClientRejectsPlainWSLegacyIdentityAndUnknownNestedField(t *testing.T) {
	key := testRelayKey(t)
	tests := []string{
		`{"relay":{"url":"ws://relay:9443","key":"` + key + `"},"herdr":{},"log":{}}`,
		`{"relay":{"url":"wss://relay:9443","key":"` + key + `","userid":"user","machine_id":"machine"},"herdr":{},"log":{}}`,
		`{"relay":{"url":"wss://relay:9443","key":"` + key + `","unknown":true},"herdr":{},"log":{}}`,
	}
	for _, raw := range tests {
		if _, err := LoadClient(writeConfig(t, raw)); err == nil {
			t.Fatalf("LoadClient() accepted %s", raw)
		}
	}
}

func TestLoadServerReadsSecretAndDefaultsCredentialPath(t *testing.T) {
	path := writeConfig(t, `{
	  "wecom": {"bot_id": "bot-1"},
	  "server": {"listen": "127.0.0.1:9443", "addr_hint": "10.1.3.4"},
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
	if loaded.Server.CredentialsFile != filepath.Join(loaded.Server.StateDir, "credentials.json") {
		t.Fatalf("credentials_file = %q, want under %q", loaded.Server.CredentialsFile, loaded.Server.StateDir)
	}
	if loaded.Server.AdminSocketPath != filepath.Join(loaded.Server.StateDir, "admin.sock") {
		t.Fatalf("admin socket = %q, want under %q", loaded.Server.AdminSocketPath, loaded.Server.StateDir)
	}
}

func TestLoadServerAdminDoesNotRequireWeComSecret(t *testing.T) {
	stateDir := shortConfigStateDir(t)
	path := writeConfig(t, `{
	  "wecom": {"bot_id": ""},
	  "server": {"state_dir": "`+filepath.ToSlash(stateDir)+`"},
	  "log": {}
	}`)
	loaded, err := LoadServerAdmin(path)
	if err != nil {
		t.Fatalf("LoadServerAdmin() error = %v", err)
	}
	if filepath.Base(loaded.Server.CredentialsFile) != "credentials.json" {
		t.Fatalf("LoadServerAdmin() = %#v", loaded.Server)
	}
	if loaded.Server.AdminSocketPath != filepath.Join(loaded.Server.StateDir, "admin.sock") {
		t.Fatalf("LoadServerAdmin() admin socket = %q", loaded.Server.AdminSocketPath)
	}
}

func TestLoadServerAdminRejectsInvalidDerivedAdminSocketPath(t *testing.T) {
	tests := []struct {
		name     string
		stateDir string
		want     string
	}{
		{name: "nul", stateDir: filepath.Join(t.TempDir(), "bad\x00dir"), want: "state_dir"},
		{name: "too long", stateDir: filepath.Join(t.TempDir(), strings.Repeat("x", 120)), want: "admin.sock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.json")
			quotedStateDir, marshalErr := json.Marshal(test.stateDir)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			raw := `{"wecom":{},"server":{"state_dir":` + string(quotedStateDir) + `},"log":{}}`
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadServerAdmin(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadServerAdmin() error = %v, want %q", err, test.want)
			}
		})
	}
}

func shortConfigStateDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "hp-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(path) })
	return path
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

func testRelayKey(t *testing.T) string {
	t.Helper()
	token, _, err := credential.Issue(1, "user", "machine", []credential.SourceRule{"127.0.0.1"}, nil, time.Unix(1, 0), bytes.NewReader(bytes.Repeat([]byte{0x31}, 48)))
	if err != nil {
		t.Fatalf("credential.Issue() error = %v", err)
	}
	return token
}
