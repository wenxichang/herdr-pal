package config

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	if loaded.RateLimit.PerSecond != 1 || loaded.RateLimit.PerMinute != 20 {
		t.Fatalf("rate limit defaults = %#v", loaded.RateLimit)
	}
	if loaded.Audit.Type != "none" || loaded.Audit.Stderr || len(loaded.Audit.Headers) != 0 {
		t.Fatalf("audit defaults = %#v", loaded.Audit)
	}
}

func TestLoadServerAcceptsExplicitRateLimitDisable(t *testing.T) {
	path := writeConfig(t, `{
  "wecom": {"bot_id": "bot-1"},
  "server": {"listen": "127.0.0.1:9443"},
  "rate_limit": {"per_second": 0, "per_minute": 0},
  "audit": {"type": "none", "stderr": true},
  "log": {}
}`)
	loaded, err := LoadServer(path, func(string) string { return "secret-value" })
	if err != nil {
		t.Fatalf("LoadServer() error = %v", err)
	}
	if loaded.RateLimit.PerSecond != 0 || loaded.RateLimit.PerMinute != 0 {
		t.Fatalf("rate limit = %#v", loaded.RateLimit)
	}
	if loaded.Audit.Type != "none" || !loaded.Audit.Stderr {
		t.Fatalf("audit = %#v", loaded.Audit)
	}
}

func TestLoadServerRejectsInvalidRateLimit(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value int
	}{
		{name: "negative second", field: "per_second", value: -1},
		{name: "large second", field: "per_second", value: 10001},
		{name: "negative minute", field: "per_minute", value: -1},
		{name: "large minute", field: "per_minute", value: 10001},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443"},"rate_limit":{"` + test.field + `":` + fmt.Sprint(test.value) + `},"log":{}}`
			_, err := LoadServer(writeConfig(t, raw), func(string) string { return "secret" })
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("LoadServer() error = %v, want %q", err, test.field)
			}
		})
	}
}

func TestLoadServerAcceptsOTLPAuditAndParsesHeaders(t *testing.T) {
	path := writeConfig(t, `{
  "wecom": {"bot_id": "bot-1"},
  "server": {"listen": "127.0.0.1:9443"},
  "audit": {
    "type": "otlp",
    "endpoint": "https://loki.example:3100/otlp/v1/logs",
    "skip_verify": true,
    "stderr": true
  },
  "log": {}
}`)
	loaded, err := LoadServer(path, func(name string) string {
		switch name {
		case SecretEnvName:
			return "secret-value"
		case OTLPLogsHeadersEnvName:
			return "Authorization=Bearer%20token,x-tenant=team%2Cone"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("LoadServer() error = %v", err)
	}
	if loaded.Audit.Type != "otlp" || loaded.Audit.Endpoint != "https://loki.example:3100/otlp/v1/logs" || !loaded.Audit.SkipVerify || !loaded.Audit.Stderr {
		t.Fatalf("audit = %#v", loaded.Audit)
	}
	if loaded.Audit.Headers["Authorization"] != "Bearer token" || loaded.Audit.Headers["X-Tenant"] != "team,one" {
		t.Fatalf("headers = %#v", loaded.Audit.Headers)
	}
}

func TestLoadServerRejectsInvalidAudit(t *testing.T) {
	tests := []struct {
		name  string
		audit string
		want  string
	}{
		{name: "unknown type", audit: `{"type":"file"}`, want: "audit.type"},
		{name: "missing endpoint", audit: `{"type":"otlp"}`, want: "audit.endpoint"},
		{name: "userinfo", audit: `{"type":"otlp","endpoint":"https://user:pass@collector/v1/logs"}`, want: "userinfo"},
		{name: "query", audit: `{"type":"otlp","endpoint":"https://collector/v1/logs?q=1"}`, want: "query"},
		{name: "fragment", audit: `{"type":"otlp","endpoint":"https://collector/v1/logs#x"}`, want: "fragment"},
		{name: "wrong scheme", audit: `{"type":"otlp","endpoint":"ftp://collector/v1/logs"}`, want: "http"},
		{name: "http skip verify", audit: `{"type":"otlp","endpoint":"http://collector/v1/logs","skip_verify":true}`, want: "skip_verify"},
		{name: "none endpoint", audit: `{"type":"none","endpoint":"https://collector/v1/logs"}`, want: "audit.endpoint"},
		{name: "none skip verify", audit: `{"type":"none","skip_verify":true}`, want: "skip_verify"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443"},"audit":` + test.audit + `,"log":{}}`
			_, err := LoadServer(writeConfig(t, raw), func(string) string { return "secret" })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadServer() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadServerRejectsMalformedOTLPHeaders(t *testing.T) {
	path := writeConfig(t, `{
  "wecom": {"bot_id": "bot-1"},
  "server": {"listen": "127.0.0.1:9443"},
  "audit": {"type": "otlp", "endpoint": "https://collector/v1/logs"},
  "log": {}
}`)
	_, err := LoadServer(path, func(name string) string {
		if name == SecretEnvName {
			return "secret"
		}
		return "missing-equals"
	})
	if err == nil || !strings.Contains(err.Error(), OTLPLogsHeadersEnvName) || strings.Contains(err.Error(), "missing-equals") {
		t.Fatalf("LoadServer() error = %v", err)
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
