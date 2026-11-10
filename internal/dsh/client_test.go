package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// sessionListCapture 保存一次 session/list 请求的关键字段，供测试断言调用协议。
type sessionListCapture struct {
	method string
	path   string
	args   json.RawMessage
}

// newSessionsTestClient 构造真实 RPC 客户端与回环测试宿主，并返回 Client。
//
// handler 只负责构造 result.value；items 为 nil 时返回空目录。
// 客户端复用 rpcTestFixture 的认证文件与固定时钟，避免测试依赖本机 dsh。
func newSessionsTestClient(t *testing.T, items any) (*Client, <-chan sessionListCapture) {
	t.Helper()
	captured := make(chan sessionListCapture, 1)
	responseItems := any([]any{})
	if items != nil {
		responseItems = items
	}
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		var request struct {
			RPCID   string `json:"rpcId"`
			Payload struct {
				Args json.RawMessage `json:"args"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("解析请求帧: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		select {
		case captured <- sessionListCapture{method: r.Method, path: r.URL.Path, args: request.Payload.Args}:
		default:
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": request.RPCID,
			"result": map[string]any{
				"ok":    true,
				"value": map[string]any{"items": responseItems},
			},
		})
	})

	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if client == nil {
		t.Fatal("NewClient 返回 nil")
	}
	// 复用 fixture 的固定时钟，保证认证 Cookie 可复现。
	client.rpc = fixture.client
	return client, captured
}

// newSessionsErrorClient 构造固定返回 ok=false 的宿主响应，验证错误透传。
func newSessionsErrorClient(t *testing.T) *Client {
	t.Helper()
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		rpcID := readRPCID(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"auth-required","message":"denied"}}}`,
			rpcID,
		)
	})
	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client
	return client
}

func TestSessionsFiltersUncontrolledAndSubagent(t *testing.T) {
	items := []map[string]any{
		{
			"sessionId":      "owned",
			"agentAvailable": true,
			"running":        true,
			"updatedAt":      3,
			"cwd":            "/work/owned",
			"projections":    map[string]any{"values": map[string]any{"title": "正常会话"}},
		},
		{
			"sessionId":      "cold-history",
			"agentAvailable": false,
			"running":        false,
			"updatedAt":      2,
		},
		{
			// 缺失控制权字段时同样不可列出，避免协议漂移导致越权展示。
			"sessionId": "unknown-ownership",
			"running":   false,
			"updatedAt": 2,
		},
		{
			"sessionId":      "subagent-child",
			"agentAvailable": true,
			"origin":         "subagent",
			"running":        false,
			"updatedAt":      1,
		},
		{
			"sessionId":      "migrating",
			"agentAvailable": true,
			"formatStatus":   "migration-required",
			"running":        false,
			"updatedAt":      0,
		},
	}

	client, captured := newSessionsTestClient(t, items)
	sessions, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("读取会话目录: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("过滤后会话数 = %d，期望 1: %+v", len(sessions), sessions)
	}
	if sessions[0].ID != "owned" || sessions[0].Title != "正常会话" || !sessions[0].Available || !sessions[0].Running {
		t.Errorf("保留会话字段错误: %+v", sessions[0])
	}

	request := <-captured
	if request.method != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", request.method)
	}
	if request.path != "/api/session/list" {
		t.Errorf("请求路径 = %q，期望 /api/session/list", request.path)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(request.args, &args); err != nil {
		t.Fatalf("解析 payload.args: %v", err)
	}
	requestArg, ok := args["_request"]
	if !ok || string(requestArg) != "{}" {
		t.Errorf("payload.args._request = %s，期望空对象", requestArg)
	}
}

func TestSessionsCopiesTitleAndWorkspace(t *testing.T) {
	items := []map[string]any{
		{
			"sessionId":      "with-title",
			"agentAvailable": true,
			"running":        true,
			"updatedAt":      10,
			"cwd":            "/Users/wxc/Code/herdr-pal",
			"projections":    map[string]any{"values": map[string]any{"title": "设计讨论"}},
		},
		{
			"sessionId":      "without-title",
			"agentAvailable": true,
			"running":        false,
			"updatedAt":      20,
		},
	}

	client, _ := newSessionsTestClient(t, items)
	sessions, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("读取会话目录: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("会话数 = %d，期望 2", len(sessions))
	}

	byID := map[string]SessionSummary{}
	for _, session := range sessions {
		byID[session.ID] = session
	}

	withTitle, ok := byID["with-title"]
	if !ok {
		t.Fatal("缺少 with-title 会话")
	}
	if withTitle.Title != "设计讨论" {
		t.Errorf("Title = %q，期望投影标题", withTitle.Title)
	}
	if withTitle.Cwd != "/Users/wxc/Code/herdr-pal" {
		t.Errorf("Cwd = %q，期望原始 cwd 值（不是 basename）", withTitle.Cwd)
	}
	if !withTitle.Running || !withTitle.Available || withTitle.UpdatedAt != 10 {
		t.Errorf("with-title 状态字段错误: %+v", withTitle)
	}

	withoutTitle, ok := byID["without-title"]
	if !ok {
		t.Fatal("缺少 without-title 会话")
	}
	if withoutTitle.Title != "without-title" {
		t.Errorf("Title = %q，期望缺省为会话 ID", withoutTitle.Title)
	}
	if withoutTitle.Cwd != "" {
		t.Errorf("Cwd = %q，期望空字符串", withoutTitle.Cwd)
	}
}

func TestSessionsBoundsAndValidatesIDs(t *testing.T) {
	t.Run("空ID", func(t *testing.T) {
		client, _ := newSessionsTestClient(t, []map[string]any{
			{"sessionId": "", "agentAvailable": true},
		})
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionIDInvalid) {
			t.Fatalf("空 ID 应返回 errSessionIDInvalid，实际 %v", err)
		}
	})

	t.Run("超长ID", func(t *testing.T) {
		client, _ := newSessionsTestClient(t, []map[string]any{
			{"sessionId": strings.Repeat("a", 513), "agentAvailable": true},
		})
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionIDInvalid) {
			t.Fatalf("超过 512 字节的 ID 应返回 errSessionIDInvalid，实际 %v", err)
		}
	})

	t.Run("控制字符ID", func(t *testing.T) {
		client, _ := newSessionsTestClient(t, []map[string]any{
			{"sessionId": "bad\nid", "agentAvailable": true},
		})
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionIDInvalid) {
			t.Fatalf("含控制字符的 ID 应返回 errSessionIDInvalid，实际 %v", err)
		}
	})

	t.Run("重复ID", func(t *testing.T) {
		client, _ := newSessionsTestClient(t, []map[string]any{
			{"sessionId": "dup", "agentAvailable": true},
			{"sessionId": "dup", "agentAvailable": false},
		})
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionIDDuplicate) {
			t.Fatalf("重复 ID 应返回 errSessionIDDuplicate，实际 %v", err)
		}
	})

	t.Run("超过8192条原始行", func(t *testing.T) {
		items := make([]map[string]any, 8193)
		for i := range items {
			items[i] = map[string]any{
				"sessionId":      fmt.Sprintf("s-%05d", i),
				"agentAvailable": false,
			}
		}
		client, _ := newSessionsTestClient(t, items)
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionListTooMany) {
			t.Fatalf("超过 8192 条应返回 errSessionListTooMany，实际 %v", err)
		}
	})

	t.Run("恰好8192条可过滤行", func(t *testing.T) {
		items := make([]map[string]any, 8192)
		for i := range items {
			items[i] = map[string]any{
				"sessionId":      fmt.Sprintf("s-%05d", i),
				"agentAvailable": false,
			}
		}
		client, _ := newSessionsTestClient(t, items)
		sessions, err := client.Sessions(context.Background())
		if err != nil {
			t.Fatalf("8192 条原始行不应失败: %v", err)
		}
		if len(sessions) != 0 {
			t.Fatalf("全部不可控制时应返回空目录，实际 %d 条", len(sessions))
		}
	})

	t.Run("超过256个可控制会话", func(t *testing.T) {
		items := make([]map[string]any, 257)
		for i := range items {
			items[i] = map[string]any{
				"sessionId":      fmt.Sprintf("s-%05d", i),
				"agentAvailable": true,
			}
		}
		client, _ := newSessionsTestClient(t, items)
		_, err := client.Sessions(context.Background())
		if !errors.Is(err, errSessionCapacity) {
			t.Fatalf("超过 256 个可控制会话应返回 errSessionCapacity，实际 %v", err)
		}
	})

	t.Run("恰好256个可控制会话", func(t *testing.T) {
		items := make([]map[string]any, 256)
		for i := range items {
			items[i] = map[string]any{
				"sessionId":      fmt.Sprintf("s-%05d", i),
				"agentAvailable": true,
			}
		}
		client, _ := newSessionsTestClient(t, items)
		sessions, err := client.Sessions(context.Background())
		if err != nil {
			t.Fatalf("256 个可控制会话不应失败: %v", err)
		}
		if len(sessions) != 256 {
			t.Fatalf("会话数 = %d，期望 256", len(sessions))
		}
	})
}

func TestSessionsSortsDeterministically(t *testing.T) {
	items := []map[string]any{
		{"sessionId": "beta", "agentAvailable": true, "updatedAt": 100},
		{"sessionId": "oldest", "agentAvailable": true, "updatedAt": 50},
		{"sessionId": "newest", "agentAvailable": true, "updatedAt": 200},
		{"sessionId": "alpha", "agentAvailable": true, "updatedAt": 100},
	}

	client, _ := newSessionsTestClient(t, items)
	first, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("读取会话目录: %v", err)
	}
	second, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("再次读取会话目录: %v", err)
	}

	want := []string{"newest", "alpha", "beta", "oldest"}
	for call, got := range [][]SessionSummary{first, second} {
		if len(got) != len(want) {
			t.Fatalf("第 %d 次会话数 = %d，期望 %d", call+1, len(got), len(want))
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("第 %d 次排序 = %v，期望 %v", call+1, sessionIDs(got), want)
			}
		}
	}
}

func TestSessionsPropagatesRPCError(t *testing.T) {
	client := newSessionsErrorClient(t)
	_, err := client.Sessions(context.Background())
	if err == nil {
		t.Fatal("RPC 业务错误应返回错误")
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != "auth-required" {
		t.Fatalf("错误应保留 rpcError 错误码，实际 %v", err)
	}
}

// sessionIDs 提取会话 ID 序列，便于输出可读的排序断言。
func sessionIDs(sessions []SessionSummary) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}
