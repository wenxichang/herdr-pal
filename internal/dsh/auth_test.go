package dsh

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	testCookieAuthority = "127.0.0.1:3080"
	testCookieNowMs     = int64(1_700_000_000_000)
	testSecretBytes     = 32
)

// testCredentialYAML 生成与 dsh 私有认证记录一致的固定格式，并保留 refs 等未知字段。
func testCredentialYAML(secret string) string {
	return fmt.Sprintf(`version: 1
records:
  client-connection/browser-session:
    kind: grant
    payload:
      version: 1
      secret: %s
refs:
  private-model-key: do-not-use
`, secret)
}

func writeTestCredential(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写入测试认证文件: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("设置测试认证文件权限: %v", err)
	}
	return path
}

func testZeroSecret() ([]byte, string) {
	raw := make([]byte, testSecretBytes)
	return raw, base64.RawURLEncoding.EncodeToString(raw)
}

func TestCookieIsBoundToAuthorityAndExpiry(t *testing.T) {
	secret, encoded := testZeroSecret()
	path := writeTestCredential(t, testCredentialYAML(encoded), 0o600)
	now := time.UnixMilli(testCookieNowMs)

	cookie, err := cookieForCredentials(path, testCookieAuthority, now)
	if err != nil {
		t.Fatalf("生成 Cookie: %v", err)
	}
	again, err := cookieForCredentials(path, testCookieAuthority, now)
	if err != nil {
		t.Fatalf("重复生成 Cookie: %v", err)
	}
	if cookie != again {
		t.Fatal("同一 authority 与时间应生成相同 Cookie")
	}

	other, err := cookieForCredentials(path, "127.0.0.1:3081", now)
	if err != nil {
		t.Fatalf("生成另一 authority 的 Cookie: %v", err)
	}
	if cookie == other {
		t.Fatal("不同 authority 应生成不同 Cookie")
	}

	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("Cookie 缺少 name=value 分隔符: %q", cookie)
	}
	otherName, _, ok := strings.Cut(other, "=")
	if !ok || name == otherName {
		t.Fatal("不同 authority 的 Cookie 名称应不同")
	}
	wantNameHash := sha256.Sum256([]byte(testCookieAuthority))
	wantName := "dsh-auth-" + base64.RawURLEncoding.EncodeToString(wantNameHash[:])
	if name != wantName {
		t.Fatalf("Cookie 名称错误: got %q want %q", name, wantName)
	}

	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("Cookie 值格式错误: %q", value)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解码 Cookie 载荷: %v", err)
	}
	wantPayload := fmt.Sprintf(
		`{"version":1,"authority":%q,"issuedAt":%d,"expiresAt":%d}`,
		testCookieAuthority, testCookieNowMs, testCookieNowMs+300000,
	)
	if string(payloadJSON) != wantPayload {
		t.Fatalf("Cookie 载荷错误: got %s want %s", payloadJSON, wantPayload)
	}
	var payload struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("解析 Cookie 载荷: %v", err)
	}
	if payload.Version != 1 || payload.Authority != testCookieAuthority ||
		payload.IssuedAt != testCookieNowMs || payload.ExpiresAt != testCookieNowMs+300000 {
		t.Fatalf("Cookie 载荷字段错误: %+v", payload)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[1]))
	wantSignature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if parts[2] != wantSignature {
		t.Fatal("Cookie 签名与认证密钥不匹配")
	}
}

func TestCookieRejectsInvalidAuthority(t *testing.T) {
	_, encoded := testZeroSecret()
	path := writeTestCredential(t, testCredentialYAML(encoded), 0o600)
	now := time.UnixMilli(testCookieNowMs)
	for _, authority := range []string{
		"",
		"localhost:3080",
		"127.0.0.1",
		"127.0.0.1:",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"127.0.0.1:abc",
		"http://127.0.0.1:3080",
		"[::1]:3080",
	} {
		if _, err := cookieForCredentials(path, authority, now); err == nil {
			t.Errorf("authority %q 应被拒绝", authority)
		}
	}
}

func TestCookieRejectsPublicFileAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位；符号链接与 ACL 行为由平台分支单独处理")
	}
	_, encoded := testZeroSecret()
	now := time.UnixMilli(testCookieNowMs)

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		path := writeTestCredential(t, testCredentialYAML(encoded), mode)
		if _, err := cookieForCredentials(path, testCookieAuthority, now); err == nil {
			t.Errorf("权限 %#o 的认证文件应被拒绝", mode)
		}
	}

	secure := writeTestCredential(t, testCredentialYAML(encoded), 0o600)
	link := filepath.Join(t.TempDir(), "credentials-link.yaml")
	if err := os.Symlink(secure, link); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}
	if _, err := cookieForCredentials(link, testCookieAuthority, now); err == nil {
		t.Fatal("符号链接认证文件应被拒绝")
	}

	if _, err := cookieForCredentials(t.TempDir(), testCookieAuthority, now); err == nil {
		t.Fatal("目录不能作为认证文件")
	}
}

func TestCookieRejectsOversizedCredentialFile(t *testing.T) {
	_, encoded := testZeroSecret()
	content := testCredentialYAML(encoded) + strings.Repeat("#", 256*1024)
	path := writeTestCredential(t, content, 0o600)
	if _, err := cookieForCredentials(path, testCookieAuthority, time.UnixMilli(testCookieNowMs)); err == nil {
		t.Fatal("超过 256 KiB 的认证文件应被拒绝")
	}
}

func TestCookieRejectsUnknownCredentialVersions(t *testing.T) {
	_, encoded := testZeroSecret()
	valid := testCredentialYAML(encoded)
	cases := []struct {
		name    string
		content string
	}{
		{"顶层版本", strings.Replace(valid, "version: 1\nrecords:", "version: 2\nrecords:", 1)},
		{"记录类型", strings.Replace(valid, "kind: grant", "kind: other", 1)},
		{"载荷版本", strings.Replace(valid, "      version: 1\n      secret:", "      version: 2\n      secret:", 1)},
		{"密钥过短", testCredentialYAML(base64.RawURLEncoding.EncodeToString(make([]byte, 31)))},
		{"密钥过长", testCredentialYAML(base64.RawURLEncoding.EncodeToString(make([]byte, 33)))},
		{"密钥带填充", testCredentialYAML(encoded + "=")},
		{"密钥非 base64url", testCredentialYAML("not*base64!")},
		{"缺少记录", "version: 1\nrecords: {}\n"},
		{"非 YAML", "not: [valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestCredential(t, tc.content, 0o600)
			if _, err := cookieForCredentials(path, testCookieAuthority, time.UnixMilli(testCookieNowMs)); err == nil {
				t.Fatal("不兼容的认证记录应被拒绝")
			}
		})
	}
}

func TestCookieErrorDoesNotContainSecret(t *testing.T) {
	marker := []byte("S3cret!S3cret!S3cret!S3cret!ABCD")
	if len(marker) != testSecretBytes {
		t.Fatalf("测试密钥长度错误: %d", len(marker))
	}
	encoded := base64.RawURLEncoding.EncodeToString(marker)
	now := time.UnixMilli(testCookieNowMs)

	path := writeTestCredential(t, testCredentialYAML(encoded), 0o600)
	cookie, err := cookieForCredentials(path, testCookieAuthority, now)
	if err != nil {
		t.Fatalf("生成 Cookie: %v", err)
	}
	for _, leak := range []string{string(marker), encoded} {
		if strings.Contains(cookie, leak) {
			t.Fatal("Cookie 不应包含认证密钥")
		}
	}

	bad := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"公开文件", testCredentialYAML(encoded), 0o644},
		{"顶层版本", strings.Replace(testCredentialYAML(encoded), "version: 1\nrecords:", "version: 2\nrecords:", 1), 0o600},
		{"记录类型", strings.Replace(testCredentialYAML(encoded), "kind: grant", "kind: other", 1), 0o600},
		{"密钥长度", testCredentialYAML(base64.RawURLEncoding.EncodeToString(marker[:31])), 0o600},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode != 0o600 && runtime.GOOS == "windows" {
				t.Skip("Windows 不使用 Unix 权限位")
			}
			badPath := writeTestCredential(t, tc.content, tc.mode)
			_, err := cookieForCredentials(badPath, testCookieAuthority, now)
			if err == nil {
				t.Fatal("认证记录应被拒绝")
			}
			if strings.Contains(err.Error(), string(marker)) || strings.Contains(err.Error(), encoded) {
				t.Fatal("错误信息不应包含认证密钥")
			}
		})
	}
}
