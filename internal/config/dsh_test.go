package config

import (
	"strings"
	"testing"
)

func TestDSHValidation(t *testing.T) {
	for _, test := range []struct {
		name, value string
		ok          bool
	}{
		{"missing backend", `{"herdr":{"disabled":true}}`, false},
		{"remote", `{"dsh":{"url":"http://example.com:123","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, false},
		{"dsh only", `{"herdr":{"disabled":true},"dsh":{"url":"http://127.0.0.1:17842","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, true},
		{"mixed", `{"dsh":{"url":"http://127.0.0.1:17842","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, true},
		{"short token", `{"dsh":{"url":"http://127.0.0.1:17842","token":"short"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadDSHClientConfig(t, test.value); (err == nil) != test.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDSHConfigNativeMode(t *testing.T) {
	for _, test := range []struct {
		name, value string
	}{
		{"仅配置 dsh", `{"dsh":{}}`},
		{"禁用 Herdr 时配置空 dsh", `{"herdr":{"disabled":true},"dsh":{}}`},
		{"配置原生字段", `{"herdr":{"disabled":true},"dsh":{"url":"http://127.0.0.1:17842","home":"/tmp/dsh-home","variant":"web"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, err := loadDSHClientConfig(t, test.value)
			if err != nil {
				t.Fatalf("LoadClient() error = %v", err)
			}
			if loaded.DSH == nil {
				t.Fatal("DSH 指针不应为 nil")
			}
		})
	}

	t.Run("禁用 Herdr 但缺少 dsh", func(t *testing.T) {
		if _, err := loadDSHClientConfig(t, `{"herdr":{"disabled":true}}`); err == nil {
			t.Fatal("缺少 dsh 时禁用 Herdr 应失败")
		}
	})
}

func TestDSHConfigKeepsLegacyTokenDuringMigration(t *testing.T) {
	loaded, err := loadDSHClientConfig(t, `{"dsh":{"token":"x"}}`)
	if err != nil {
		t.Fatalf("过渡期仅 token 应可加载：%v", err)
	}
	if loaded.DSH == nil || loaded.DSH.Token != "x" {
		t.Fatalf("DSH = %#v，期望保留旧 token", loaded.DSH)
	}
}

func TestDSHConfigValidatesURLHomeAndVariant(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"远程 URL", `{"dsh":{"url":"http://example.com:1"}}`, "dsh.url"},
		{"URL 带路径", `{"dsh":{"url":"http://127.0.0.1:1/path"}}`, "dsh.url"},
		{"URL 带查询", `{"dsh":{"url":"http://127.0.0.1:1?x=1"}}`, "dsh.url"},
		{"URL 带凭据", `{"dsh":{"url":"http://user@127.0.0.1:1"}}`, "dsh.url"},
		{"URL 缺少端口", `{"dsh":{"url":"http://127.0.0.1"}}`, "dsh.url"},
		{"相对 home", `{"dsh":{"home":"relative/home"}}`, "dsh.home"},
		{"未知 variant", `{"dsh":{"variant":"other"}}`, "dsh.variant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadDSHClientConfig(t, test.value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadClient() error = %v，期望包含 %q", err, test.want)
			}
		})
	}

	for _, value := range []string{
		`{"dsh":{"url":"http://127.0.0.1:17842"}}`,
		`{"dsh":{"home":"/tmp/dsh-home"}}`,
		`{"dsh":{"variant":""}}`,
		`{"dsh":{"variant":"auto"}}`,
		`{"dsh":{"variant":"web"}}`,
		`{"dsh":{"variant":"desktop"}}`,
	} {
		if _, err := loadDSHClientConfig(t, value); err != nil {
			t.Fatalf("LoadClient() 拒绝了合法配置 %s：%v", value, err)
		}
	}
}

func loadDSHClientConfig(t *testing.T, value string) (ClientConfig, error) {
	t.Helper()
	key := testRelayKey(t)
	raw := `{"relay":{"url":"wss://relay.internal:9443","key":"` + key + `"},` + strings.TrimPrefix(value, "{")
	return LoadClient(writeConfig(t, raw))
}
