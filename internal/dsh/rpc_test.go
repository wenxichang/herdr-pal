package dsh

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rpcTestFixture 提供回环测试服务器、临时 DSH_HOME 与注入固定时钟的 RPC 客户端。
type rpcTestFixture struct {
	server          *httptest.Server
	client          *rpcClient
	authority       string
	credentialsPath string
}

func newRPCTestFixture(t *testing.T, handler http.HandlerFunc) rpcTestFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	authority := strings.TrimPrefix(server.URL, "http://")
	_, portText, err := net.SplitHostPort(authority)
	if err != nil {
		t.Fatalf("解析测试服务器地址 %q: %v", server.URL, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("测试服务器端口无效: %q", portText)
	}

	_, encoded := testZeroSecret()
	home := t.TempDir()
	credentialsPath := filepath.Join(home, ".credentials.yaml")
	if err := os.WriteFile(credentialsPath, []byte(testCredentialYAML(encoded)), 0o600); err != nil {
		t.Fatalf("写入测试认证文件: %v", err)
	}
	if err := os.Chmod(credentialsPath, 0o600); err != nil {
		t.Fatalf("设置测试认证文件权限: %v", err)
	}

	client := newRPCClient(Host{
		Port:    uint16(port),
		Version: "explicit",
		Variant: variantWeb,
		Home:    home,
	})
	client.now = func() time.Time { return time.UnixMilli(testCookieNowMs) }

	return rpcTestFixture{
		server:          server,
		client:          client,
		authority:       authority,
		credentialsPath: credentialsPath,
	}
}

// readRPCID 从客户端请求帧中取出 rpcId，供处理器构造关联响应。
func readRPCID(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("读取请求体: %v", err)
		return ""
	}
	var request struct {
		RPCID string `json:"rpcId"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Errorf("解析请求帧: %v", err)
		return ""
	}
	return request.RPCID
}

func TestRPCSendsCorrelatedRequest(t *testing.T) {
	type captured struct {
		method      string
		path        string
		contentType string
		cookie      string
		body        []byte
	}
	capturedCh := make(chan captured, 1)

	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		capturedCh <- captured{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			cookie:      r.Header.Get("Cookie"),
			body:        body,
		}
		var request struct {
			RPCID string `json:"rpcId"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("解析请求帧: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"type":"server-response","rpcId":%q,"result":{"ok":true,"value":{"items":[]}}}`,
			request.RPCID,
		)
	})

	args := map[string]any{"_request": map[string]any{}}
	value, err := fixture.client.call(context.Background(), "session/list", args)
	if err != nil {
		t.Fatalf("调用 session/list: %v", err)
	}

	got := <-capturedCh
	if got.method != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", got.method)
	}
	if got.path != "/api/session/list" {
		t.Errorf("请求路径 = %q，期望 /api/session/list", got.path)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", got.contentType)
	}

	wantCookie, err := cookieForCredentials(fixture.credentialsPath, fixture.authority, time.UnixMilli(testCookieNowMs))
	if err != nil {
		t.Fatalf("生成期望 Cookie: %v", err)
	}
	if got.cookie == "" {
		t.Fatal("请求缺少 Cookie")
	}
	if got.cookie != wantCookie {
		t.Errorf("Cookie 与认证记录生成的期望值不一致")
	}

	var request struct {
		Type    string `json:"type"`
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args json.RawMessage `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(got.body, &request); err != nil {
		t.Fatalf("解析请求 JSON: %v", err)
	}
	if request.Type != "client-request" {
		t.Errorf("请求 type = %q，期望 client-request", request.Type)
	}
	if request.Method != "session/list" {
		t.Errorf("请求 method = %q，期望 session/list", request.Method)
	}
	if len(request.RPCID) != 32 {
		t.Errorf("rpcId 长度 = %d，期望 32 位十六进制", len(request.RPCID))
	}
	if _, err := hex.DecodeString(request.RPCID); err != nil {
		t.Errorf("rpcId 不是十六进制: %q", request.RPCID)
	}
	var decodedArgs map[string]any
	if err := json.Unmarshal(request.Payload.Args, &decodedArgs); err != nil {
		t.Fatalf("解析 payload.args: %v", err)
	}
	if _, ok := decodedArgs["_request"].(map[string]any); !ok {
		t.Errorf("payload.args = %s，期望包含 _request 对象", request.Payload.Args)
	}

	var decodedValue map[string]any
	if err := json.Unmarshal(value, &decodedValue); err != nil {
		t.Fatalf("解析返回值: %v", err)
	}
	items, ok := decodedValue["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("返回值 = %s，期望 items 为空数组", value)
	}

	if fixture.client.http.Timeout != 5*time.Second {
		t.Errorf("HTTP 超时 = %v，期望 5s", fixture.client.http.Timeout)
	}
	transport, ok := fixture.client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，期望 *http.Transport", fixture.client.http.Transport)
	}
	if transport.Proxy != nil {
		t.Error("HTTP Transport 必须禁用代理")
	}
}

func TestRPCRejectsMismatchedRPCID(t *testing.T) {
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if rpcID := readRPCID(t, r); rpcID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w,
			`{"type":"server-response","rpcId":"00000000000000000000000000000000","result":{"ok":true,"value":{}}}`,
		)
	})

	if _, err := fixture.client.call(context.Background(), "session/list", nil); !errors.Is(err, errRPCMismatchedID) {
		t.Fatalf("rpcId 不匹配应返回 errRPCMismatchedID，实际 %v", err)
	}
}

func TestRPCRejectsUnknownEndpoint(t *testing.T) {
	var hits atomic.Int32
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, `{"type":"server-response","rpcId":"x","result":{"ok":true,"value":null}}`)
	})

	for _, endpoint := range []string{
		"",
		"session/delete",
		"session/list/../../admin",
		"$events/subscribe",
		"userQuestions/answer/extra",
	} {
		if _, err := fixture.client.call(context.Background(), endpoint, nil); !errors.Is(err, errRPCUnknownEndpoint) {
			t.Errorf("endpoint %q 应被白名单拒绝，实际错误 %v", endpoint, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("未知 endpoint 不应发出 HTTP 请求，实际 %d 次", hits.Load())
	}
}

func TestRPCBoundsResponseAndDisablesRedirect(t *testing.T) {
	t.Run("响应超过4MiB", func(t *testing.T) {
		fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_ = readRPCID(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte("x"), (4<<20)+1))
		})
		if _, err := fixture.client.call(context.Background(), "session/list", nil); !errors.Is(err, errRPCResponseTooLarge) {
			t.Fatalf("超过 4 MiB 的响应应返回 errRPCResponseTooLarge，实际 %v", err)
		}
	})

	t.Run("请求超过1MiB", func(t *testing.T) {
		var hits atomic.Int32
		fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_ = readRPCID(t, r)
			_, _ = fmt.Fprint(w, `{"type":"server-response","rpcId":"x","result":{"ok":true,"value":null}}`)
		})
		args := map[string]any{"text": strings.Repeat("a", 1<<20)}
		if _, err := fixture.client.call(context.Background(), "session/prompt", args); !errors.Is(err, errRPCRequestTooLarge) {
			t.Fatalf("超过 1 MiB 的请求体应返回 errRPCRequestTooLarge，实际 %v", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("超限请求不应发出 HTTP 请求，实际 %d 次", hits.Load())
		}
	})

	t.Run("不跟随重定向", func(t *testing.T) {
		var redirected atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			redirected.Add(1)
			_, _ = fmt.Fprint(w, `{"type":"server-response","rpcId":"x","result":{"ok":true,"value":null}}`)
		}))
		t.Cleanup(target.Close)

		fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_ = readRPCID(t, r)
			http.Redirect(w, r, target.URL, http.StatusFound)
		})
		if _, err := fixture.client.call(context.Background(), "session/list", nil); err == nil {
			t.Fatal("302 响应应返回错误")
		}
		if redirected.Load() != 0 {
			t.Fatalf("客户端不应跟随重定向，目标被访问 %d 次", redirected.Load())
		}
	})
}

func TestRPCFlagsUnauthorized(t *testing.T) {
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = readRPCID(t, r)
		http.Error(w, "dsh web authentication required", http.StatusUnauthorized)
	})

	_, err := fixture.client.call(context.Background(), "session/list", nil)
	if !errors.Is(err, errRPCAuth) {
		t.Fatalf("HTTP 401 应返回 errRPCAuth，实际 %v", err)
	}
}

func TestRPCRejectsMalformedResponse(t *testing.T) {
	cases := []struct {
		name    string
		body    func(rpcID string) string
		wantErr error
	}{
		{
			name:    "非JSON",
			body:    func(string) string { return "not json" },
			wantErr: errRPCMalformed,
		},
		{
			name: "响应类型错误",
			body: func(rpcID string) string {
				return fmt.Sprintf(`{"type":"server-event","rpcId":%q,"result":{"ok":true,"value":{}}}`, rpcID)
			},
			wantErr: errRPCMalformed,
		},
		{
			name: "缺少result",
			body: func(rpcID string) string {
				return fmt.Sprintf(`{"type":"server-response","rpcId":%q}`, rpcID)
			},
			wantErr: errRPCMissingResult,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
				rpcID := readRPCID(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body(rpcID))
			})
			if _, err := fixture.client.call(context.Background(), "session/list", nil); !errors.Is(err, tc.wantErr) {
				t.Fatalf("畸形响应应返回 %v，实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestRPCErrorCarriesCode(t *testing.T) {
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		rpcID := readRPCID(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"invalid_request","message":"bad args"}}}`,
			rpcID,
		)
	})

	_, err := fixture.client.call(context.Background(), "session/prompt", nil)
	if err == nil {
		t.Fatal("ok=false 应返回错误")
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("错误类型 = %T，期望 *rpcError", err)
	}
	if rpcErr.Code != "invalid_request" {
		t.Errorf("错误码 = %q，期望 invalid_request", rpcErr.Code)
	}
	if rpcErr.Message != "bad args" {
		t.Errorf("错误消息 = %q，期望 bad args", rpcErr.Message)
	}
	if !strings.Contains(err.Error(), "invalid_request") {
		t.Error("错误字符串应包含错误码")
	}

	wantCookie, cookieErr := cookieForCredentials(fixture.credentialsPath, fixture.authority, time.UnixMilli(testCookieNowMs))
	if cookieErr != nil {
		t.Fatalf("生成期望 Cookie: %v", cookieErr)
	}
	if strings.Contains(err.Error(), wantCookie) {
		t.Error("错误信息不应包含 Cookie")
	}
}

func TestRPCTruncatesErrorMessage(t *testing.T) {
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		rpcID := readRPCID(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": rpcID,
			"result": map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":    "too_long",
					"message": strings.Repeat("错", 600),
				},
			},
		})
	})

	_, err := fixture.client.call(context.Background(), "session/prompt", nil)
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("错误类型 = %T，期望 *rpcError", err)
	}
	if rpcErr.Message != strings.Repeat("错", 512) {
		t.Errorf("错误消息应截断为 512 个字符，实际 %d 个字符", len([]rune(rpcErr.Message)))
	}
}

func TestRPCRejectsMissingOrRelativeHome(t *testing.T) {
	var hits atomic.Int32
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, `{"type":"server-response","rpcId":"x","result":{"ok":true,"value":null}}`)
	})

	cases := []struct {
		home    string
		wantErr error
	}{
		{"", errRPCMissingHome},
		{"relative-home", errRPCHomeNotAbsolute},
	}
	for _, tc := range cases {
		client := newRPCClient(Host{
			Port:    fixture.client.host.Port,
			Version: "explicit",
			Variant: variantWeb,
			Home:    tc.home,
		})
		_, err := client.call(context.Background(), "session/list", nil)
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("Home=%q 时应返回 %v，实际 %v", tc.home, tc.wantErr, err)
		}
		if !strings.Contains(err.Error(), "Home") {
			t.Errorf("Home=%q 的错误应指明 Home: %v", tc.home, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("Home 无效时不应发出 HTTP 请求，实际 %d 次", hits.Load())
	}
}
