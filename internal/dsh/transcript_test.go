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
	"unicode/utf16"
	"unicode/utf8"
)

// rawRecord 把测试夹具编码成 session/page 返回的原始记录。
func rawRecord(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码测试记录: %v", err)
	}
	return data
}

// eventRecord 构造 session/page 的单条事件记录；surfaceOp 为 nil 时省略该字段。
func eventRecord(t *testing.T, seq int, eventType string, data any, surfaceOp any) json.RawMessage {
	t.Helper()
	event := map[string]any{"seq": seq, "type": eventType, "data": data}
	if surfaceOp != nil {
		event["surfaceOp"] = surfaceOp
	}
	return rawRecord(t, map[string]any{"type": "event", "event": event})
}

// textBlock 构造内容块中的普通文本块。
func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// userEvent 构造 user/message；kind 为空时省略 source，模拟来源缺失。
func userEvent(t *testing.T, seq int, kind string, blocks ...any) json.RawMessage {
	t.Helper()
	data := map[string]any{"content": blocks}
	if kind != "" {
		data["source"] = map[string]any{"kind": kind}
	}
	return eventRecord(t, seq, "user/message", data, "append")
}

// assistantEvent 构造 assistant/message；正文位于 data.message.content。
func assistantEvent(t *testing.T, seq int, blocks ...any) json.RawMessage {
	t.Helper()
	data := map[string]any{"message": map[string]any{"content": blocks}}
	return eventRecord(t, seq, "assistant/message", data, "append")
}

// replaceOp 构造 surfaceOp 替换操作。
func replaceOp(start, end int) map[string]any {
	return map[string]any{"op": "replace", "startSeq": start, "endSeq": end}
}

func TestTranscriptKeepsOnlyHumanAndAssistantText(t *testing.T) {
	records := []json.RawMessage{
		userEvent(t, 1, "user", textBlock("第一轮提问")),
		assistantEvent(t, 2, textBlock("第一轮回答")),
		// 非真实用户输入：系统/开发者注入、技能或定时上下文必须丢弃。
		userEvent(t, 3, "agent-instructions", textBlock("INTERNAL_INSTRUCTIONS")),
		userEvent(t, 4, "time-context", textBlock("INTERNAL_CLOCK")),
		userEvent(t, 5, "compact-checkpoint", textBlock("INTERNAL_COMPACTION")),
		// 来源缺失或未知的用户消息默认拒绝，不能当成用户输入。
		userEvent(t, 6, "", textBlock("UNKNOWN_SOURCE")),
		userEvent(t, 7, "goal-round", textBlock("INTERNAL_GOAL")),
		eventRecord(t, 8, "developer/message", map[string]any{
			"message": map[string]any{"content": []any{textBlock("DEVELOPER_INJECTION")}},
		}, "append"),
		eventRecord(t, 9, "tool/result", map[string]any{
			"message": map[string]any{"content": []any{textBlock("TOOL_RESULT")}},
		}, "append"),
		userEvent(t, 10, "user",
			textBlock("第二轮提问"),
			map[string]any{"type": "image", "url": "PRIVATE_IMAGE"},
		),
		assistantEvent(t, 11,
			map[string]any{"type": "reasoning", "text": "HIDDEN_REASONING"},
			textBlock("第二轮回答"),
			map[string]any{"type": "tool-call", "name": "bash", "arguments": "PRIVATE_COMMAND"},
			map[string]any{"type": "image", "url": "PRIVATE_IMAGE"},
		),
		// 空文本块不产生消息。
		assistantEvent(t, 12, textBlock("")),
	}

	got, err := conversationText(records)
	if err != nil {
		t.Fatalf("投影聊天正文: %v", err)
	}
	want := "用户：第一轮提问\n\n助手：第一轮回答\n\n用户：第二轮提问\n\n助手：第二轮回答"
	if got != want {
		t.Fatalf("聊天正文 = %q，期望 %q", got, want)
	}
	for _, leaked := range []string{
		"INTERNAL_INSTRUCTIONS", "INTERNAL_CLOCK", "INTERNAL_COMPACTION", "UNKNOWN_SOURCE",
		"INTERNAL_GOAL", "DEVELOPER_INJECTION", "TOOL_RESULT", "PRIVATE_IMAGE",
		"HIDDEN_REASONING", "PRIVATE_COMMAND",
	} {
		if strings.Contains(got, leaked) {
			t.Errorf("正文泄露了内部内容 %q: %q", leaked, got)
		}
	}
}

func TestTranscriptProjectsEachTextBlockInOrder(t *testing.T) {
	records := []json.RawMessage{
		assistantEvent(t, 1,
			textBlock("第一段"),
			map[string]any{"type": "reasoning", "text": "HIDDEN"},
			textBlock("第二段"),
		),
		userEvent(t, 2, "user", textBlock("追问")),
	}
	got, err := conversationText(records)
	if err != nil {
		t.Fatalf("投影聊天正文: %v", err)
	}
	if want := "助手：第一段\n\n助手：第二段\n\n用户：追问"; got != want {
		t.Fatalf("聊天正文 = %q，期望 %q", got, want)
	}
}

func TestTranscriptAppliesSurfaceReplacement(t *testing.T) {
	t.Run("替换范围内旧消息", func(t *testing.T) {
		records := []json.RawMessage{
			userEvent(t, 1, "user", textBlock("旧提问")),
			assistantEvent(t, 2, textBlock("旧回答")),
			userEvent(t, 3, "user", textBlock("新提问")),
			eventRecord(t, 4, "user/message", map[string]any{
				"source":  map[string]any{"kind": "user"},
				"content": []any{textBlock("替换后的提问")},
			}, replaceOp(1, 3)),
		}
		got, err := conversationText(records)
		if err != nil {
			t.Fatalf("投影聊天正文: %v", err)
		}
		if want := "用户：替换后的提问"; got != want {
			t.Fatalf("聊天正文 = %q，期望 %q", got, want)
		}
	})

	t.Run("压缩摘要替换后不展示摘要", func(t *testing.T) {
		records := []json.RawMessage{
			userEvent(t, 1, "user", textBlock("压缩前的提问")),
			assistantEvent(t, 2, textBlock("压缩前的回答")),
			eventRecord(t, 3, "user/message", map[string]any{
				"source":  map[string]any{"kind": "compact-checkpoint"},
				"content": []any{textBlock("INTERNAL_COMPACTION")},
			}, replaceOp(1, 2)),
			userEvent(t, 4, "user", textBlock("压缩后的提问")),
		}
		got, err := conversationText(records)
		if err != nil {
			t.Fatalf("投影聊天正文: %v", err)
		}
		if want := "用户：压缩后的提问"; got != want {
			t.Fatalf("聊天正文 = %q，期望 %q", got, want)
		}
	})

	t.Run("替换范围无效时失败关闭", func(t *testing.T) {
		records := []json.RawMessage{
			userEvent(t, 1, "user", textBlock("提问")),
			eventRecord(t, 2, "user/message", map[string]any{
				"source":  map[string]any{"kind": "user"},
				"content": []any{textBlock("替换")},
			}, replaceOp(1, 5)),
		}
		if _, err := conversationText(records); !errors.Is(err, errTranscriptMalformed) {
			t.Fatalf("无效替换范围应返回 errTranscriptMalformed，实际 %v", err)
		}
	})
}

func TestTranscriptStripsANSIAndControl(t *testing.T) {
	link := func(url, label, terminator string) string {
		return "\x1b]8;;" + url + terminator + label + "\x1b]8;;" + terminator
	}
	text := link("https://example.com/a", "文档链接", "\x07") +
		" 与 " + link("https://example.com/b", "另一链接", "\x1b\\") +
		" 控制\x00\x07\x7f\u009b结束\r\n下一行\t制表" +
		"\x1b[31m红色\x1b[0m"

	records := []json.RawMessage{
		assistantEvent(t, 1,
			textBlock(text),
			textBlock("\x1b[31m\x1b[0m"),
			textBlock("保留"),
		),
	}
	got, err := conversationText(records)
	if err != nil {
		t.Fatalf("投影聊天正文: %v", err)
	}
	want := "助手：文档链接 与 另一链接 控制结束\n下一行\t制表红色\n\n助手：保留"
	if got != want {
		t.Fatalf("聊天正文 = %q，期望 %q", got, want)
	}
	if strings.ContainsAny(got, "\x00\x07\x1b\x7f") {
		t.Errorf("正文仍含控制或转义字符: %q", got)
	}
}

func TestTranscriptKeepsLast100MessagesAnd16000UTF16(t *testing.T) {
	t.Run("101条消息只保留最后100条", func(t *testing.T) {
		records := make([]json.RawMessage, 0, 101)
		for index := 1; index <= 101; index++ {
			records = append(records, userEvent(t, index, "user", textBlock(fmt.Sprintf("m%d", index))))
		}
		got, err := conversationText(records)
		if err != nil {
			t.Fatalf("投影聊天正文: %v", err)
		}
		parts := strings.Split(got, "\n\n")
		if len(parts) != 100 {
			t.Fatalf("消息数 = %d，期望 100", len(parts))
		}
		if parts[0] != "用户：m2" {
			t.Errorf("首条消息 = %q，期望 用户：m2", parts[0])
		}
		if parts[len(parts)-1] != "用户：m101" {
			t.Errorf("末条消息 = %q，期望 用户：m101", parts[len(parts)-1])
		}
	})

	t.Run("截断到16000个UTF16代码单元", func(t *testing.T) {
		records := []json.RawMessage{
			assistantEvent(t, 1, textBlock(strings.Repeat("a", 20000))),
		}
		got, err := conversationText(records)
		if err != nil {
			t.Fatalf("投影聊天正文: %v", err)
		}
		if want := strings.Repeat("a", 16000); got != want {
			t.Fatalf("正文长度 = %d，期望恰好 16000 个 ASCII 字符", utf16Length(got))
		}
		if utf16Length(got) != 16000 {
			t.Fatalf("UTF-16 长度 = %d，期望 16000", utf16Length(got))
		}
	})

	t.Run("emoji边界不产生半个代理对", func(t *testing.T) {
		// 拼接后「助手：」3 单元 + 100 个 a + emoji 2 单元 + 15999 个 b；
		// 末尾 16000 单元恰好从 emoji 的低代理项开始，不能输出半个 emoji。
		text := strings.Repeat("a", 100) + "😀" + strings.Repeat("b", 15999)
		records := []json.RawMessage{assistantEvent(t, 1, textBlock(text))}
		got, err := conversationText(records)
		if err != nil {
			t.Fatalf("投影聊天正文: %v", err)
		}
		if want := strings.Repeat("b", 15999); got != want {
			t.Fatalf("正文 = %q，期望丢弃半个 emoji 后的 15999 个 b", got)
		}
		if !utf8.ValidString(got) {
			t.Fatal("截断后的正文不是合法 UTF-8")
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Fatal("截断后的正文含替换字符，说明切开了多字节字符")
		}
		if length := utf16Length(got); length > 16000 {
			t.Fatalf("UTF-16 长度 = %d，超过 16000", length)
		}
	})
}

func TestTranscriptRejectsMalformedRecords(t *testing.T) {
	cases := map[string]json.RawMessage{
		"非JSON":   json.RawMessage("{"),
		"缺少event": rawRecord(t, map[string]any{"type": "event"}),
		"缺少seq":   rawRecord(t, map[string]any{"type": "event", "event": map[string]any{"type": "user/message"}}),
		"缺少type":  rawRecord(t, map[string]any{"type": "event", "event": map[string]any{"seq": 1}}),
		"内容不是数组": rawRecord(t, map[string]any{"type": "event", "event": map[string]any{
			"seq": 1, "type": "user/message",
			"data": map[string]any{"source": map[string]any{"kind": "user"}, "content": "纯文本"},
		}}),
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := conversationText([]json.RawMessage{record}); !errors.Is(err, errTranscriptMalformed) {
				t.Fatalf("畸形记录应返回 errTranscriptMalformed，实际 %v", err)
			}
		})
	}
}

// readCall 保存一次 Read 期间的 RPC 请求，供测试断言请求协议。
type readCall struct {
	endpoint string
	args     json.RawMessage
}

// newReadTestClient 构造按 endpoint 返回 value 的宿主，并捕获请求序列。
func newReadTestClient(t *testing.T, respond func(endpoint string, args json.RawMessage) any) (*Client, <-chan readCall) {
	t.Helper()
	captured := make(chan readCall, 16)
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
		endpoint := strings.TrimPrefix(r.URL.Path, "/api/")
		select {
		case captured <- readCall{endpoint: endpoint, args: request.Payload.Args}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": request.RPCID,
			"result": map[string]any{
				"ok":    true,
				"value": respond(endpoint, request.Payload.Args),
			},
		})
	})
	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client
	return client, captured
}

// nextReadCall 读取并校验下一条请求的 endpoint。
func nextReadCall(t *testing.T, calls <-chan readCall, wantEndpoint string) readCall {
	t.Helper()
	select {
	case call := <-calls:
		if call.endpoint != wantEndpoint {
			t.Fatalf("请求 endpoint = %q，期望 %q", call.endpoint, wantEndpoint)
		}
		return call
	default:
		t.Fatalf("缺少 %s 请求", wantEndpoint)
		return readCall{}
	}
}

// decodePageRequest 解析 session/page 的 payload.args.request。
func decodePageRequest(t *testing.T, args json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var outer struct {
		Request map[string]json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(args, &outer); err != nil {
		t.Fatalf("解析 session/page 参数: %v", err)
	}
	return outer.Request
}

func TestReadProjectsTranscriptAndUsesFastPath(t *testing.T) {
	records := []json.RawMessage{
		userEvent(t, 1, "user", textBlock("你好")),
		assistantEvent(t, 2, textBlock("你好，有什么可以帮你？")),
	}
	client, calls := newReadTestClient(t, func(endpoint string, args json.RawMessage) any {
		switch endpoint {
		case "session/projections":
			return map[string]any{"kind": "sequenced", "asOfSeq": 42, "values": map[string]any{}}
		case "session/page":
			return map[string]any{"records": records, "hasMore": false}
		default:
			t.Errorf("意外的 endpoint %q", endpoint)
			return map[string]any{}
		}
	})

	got, err := client.Read(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("读取聊天正文: %v", err)
	}
	if want := "用户：你好\n\n助手：你好，有什么可以帮你？"; got != want {
		t.Fatalf("聊天正文 = %q，期望 %q", got, want)
	}

	projectionCall := nextReadCall(t, calls, "session/projections")
	var projectionArgs struct {
		Request struct {
			SessionID string `json:"sessionId"`
		} `json:"request"`
	}
	if err := json.Unmarshal(projectionCall.args, &projectionArgs); err != nil {
		t.Fatalf("解析 session/projections 参数: %v", err)
	}
	if projectionArgs.Request.SessionID != "session-1" {
		t.Errorf("projections sessionId = %q，期望 session-1", projectionArgs.Request.SessionID)
	}

	pageRequest := decodePageRequest(t, nextReadCall(t, calls, "session/page").args)
	var address struct {
		Kind      string `json:"kind"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(pageRequest["address"], &address); err != nil {
		t.Fatalf("解析 address: %v", err)
	}
	if address.Kind != "session" || address.SessionID != "session-1" {
		t.Errorf("address = %+v，期望 session/session-1", address)
	}
	if got := string(pageRequest["throughSeq"]); got != "42" {
		t.Errorf("throughSeq = %s，期望 42", got)
	}
	if got := string(pageRequest["maxMessages"]); got != "100" {
		t.Errorf("maxMessages = %s，期望 100", got)
	}
	if _, ok := pageRequest["beforeSeq"]; ok {
		t.Error("快路径不应携带 beforeSeq")
	}
}

func TestReadFallsBackToSmallPages(t *testing.T) {
	pageRecords := func(from, to int) []json.RawMessage {
		records := make([]json.RawMessage, 0, to-from+1)
		for seq := from; seq <= to; seq++ {
			records = append(records, userEvent(t, seq, "user", textBlock(fmt.Sprintf("m%d", seq))))
		}
		return records
	}

	client, calls := newReadTestClient(t, func(endpoint string, args json.RawMessage) any {
		switch endpoint {
		case "session/projections":
			return map[string]any{"kind": "sequenced", "asOfSeq": 500, "values": map[string]any{}}
		case "session/page":
			request := decodePageRequest(t, args)
			if string(request["maxMessages"]) == "100" {
				// 快路径响应超过 4 MiB 上限，触发降级。
				return map[string]any{
					"records": []any{},
					"hasMore": false,
					"padding": strings.Repeat("x", (4<<20)+64),
				}
			}
			if _, ok := request["beforeSeq"]; !ok {
				return map[string]any{"records": pageRecords(68, 99), "hasMore": true}
			}
			if string(request["beforeSeq"]) != "68" {
				t.Errorf("beforeSeq = %s，期望 68", request["beforeSeq"])
			}
			return map[string]any{"records": pageRecords(36, 67), "hasMore": false}
		default:
			t.Errorf("意外的 endpoint %q", endpoint)
			return map[string]any{}
		}
	})

	got, err := client.Read(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("读取聊天正文: %v", err)
	}
	parts := strings.Split(got, "\n\n")
	if len(parts) != 64 {
		t.Fatalf("消息数 = %d，期望 64", len(parts))
	}
	if parts[0] != "用户：m36" || parts[len(parts)-1] != "用户：m99" {
		t.Errorf("正文首尾 = %q / %q，期望 用户：m36 / 用户：m99", parts[0], parts[len(parts)-1])
	}

	nextReadCall(t, calls, "session/projections")
	fast := decodePageRequest(t, nextReadCall(t, calls, "session/page").args)
	if string(fast["maxMessages"]) != "100" {
		t.Errorf("快路径 maxMessages = %s，期望 100", fast["maxMessages"])
	}
	first := decodePageRequest(t, nextReadCall(t, calls, "session/page").args)
	if string(first["maxMessages"]) != "32" {
		t.Errorf("降级首页 maxMessages = %s，期望 32", first["maxMessages"])
	}
	if _, ok := first["beforeSeq"]; ok {
		t.Error("降级首页不应携带 beforeSeq")
	}
	second := decodePageRequest(t, nextReadCall(t, calls, "session/page").args)
	if string(second["beforeSeq"]) != "68" {
		t.Errorf("降级第二页 beforeSeq = %s，期望 68", second["beforeSeq"])
	}
}

func TestReadAppliesReplacementAcrossPages(t *testing.T) {
	client, calls := newReadTestClient(t, func(endpoint string, args json.RawMessage) any {
		switch endpoint {
		case "session/projections":
			return map[string]any{"kind": "sequenced", "asOfSeq": 500, "values": map[string]any{}}
		case "session/page":
			request := decodePageRequest(t, args)
			if string(request["maxMessages"]) == "100" {
				// 快路径分页不可信，转入降级分页。
				return map[string]any{"hasMore": false}
			}
			if rawBefore, ok := request["beforeSeq"]; ok {
				if string(rawBefore) != "100" {
					t.Errorf("beforeSeq = %s，期望 100", rawBefore)
				}
				return map[string]any{
					"records": []json.RawMessage{
						userEvent(t, 50, "user", textBlock("旧提问")),
						assistantEvent(t, 51, textBlock("旧回答")),
					},
					"hasMore": false,
				}
			}
			return map[string]any{
				"records": []json.RawMessage{
					eventRecord(t, 100, "user/message", map[string]any{
						"source":  map[string]any{"kind": "user"},
						"content": []any{textBlock("新提问")},
					}, replaceOp(50, 51)),
				},
				"hasMore": true,
			}
		default:
			t.Errorf("意外的 endpoint %q", endpoint)
			return map[string]any{}
		}
	})

	got, err := client.Read(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("读取聊天正文: %v", err)
	}
	if want := "用户：新提问"; got != want {
		t.Fatalf("跨页替换后正文 = %q，期望 %q", got, want)
	}
	nextReadCall(t, calls, "session/projections")
	nextReadCall(t, calls, "session/page")
	nextReadCall(t, calls, "session/page")
	nextReadCall(t, calls, "session/page")
}

func TestReadRejectsOversizedFallbackHistory(t *testing.T) {
	pageRequests := 0
	client, _ := newReadTestClient(t, func(endpoint string, args json.RawMessage) any {
		switch endpoint {
		case "session/projections":
			return map[string]any{"kind": "sequenced", "asOfSeq": 500, "values": map[string]any{}}
		case "session/page":
			request := decodePageRequest(t, args)
			if string(request["maxMessages"]) == "100" {
				// 快路径分页格式无效，触发降级。
				return map[string]any{"hasMore": false}
			}
			pageRequests++
			if rawBefore, ok := request["beforeSeq"]; ok {
				var before int
				if _, err := fmt.Sscanf(string(rawBefore), "%d", &before); err != nil {
					t.Errorf("解析 beforeSeq %s: %v", rawBefore, err)
				}
				if want := 500 - (pageRequests-1)*100; before != want {
					t.Errorf("第 %d 页 beforeSeq = %d，期望 %d", pageRequests, before, want)
				}
			}
			seq := 500 - pageRequests*100
			return map[string]any{
				"records": []json.RawMessage{userEvent(t, seq, "user", textBlock(fmt.Sprintf("m%d", seq)))},
				"hasMore": true,
			}
		default:
			t.Errorf("意外的 endpoint %q", endpoint)
			return map[string]any{}
		}
	})

	_, err := client.Read(context.Background(), "session-1")
	if !errors.Is(err, errHistoryTooLarge) {
		t.Fatalf("超过 4 页仍有更早历史应返回 errHistoryTooLarge，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "历史过大") {
		t.Errorf("错误信息应说明历史过大: %v", err)
	}
	if pageRequests != 4 {
		t.Fatalf("降级页请求数 = %d，期望 4", pageRequests)
	}
}

func TestReadRejectsMigrationOrMissingCut(t *testing.T) {
	cases := map[string]any{
		"迁移中":    map[string]any{"kind": "migration-required", "values": map[string]any{}},
		"缓存投影":   map[string]any{"kind": "cached", "asOfSeq": 5, "values": map[string]any{}},
		"空投影":    nil,
		"缺少切点":   map[string]any{"kind": "sequenced", "values": map[string]any{}},
		"切点小于-1": map[string]any{"kind": "sequenced", "asOfSeq": -2, "values": map[string]any{}},
		"切点不是整数": map[string]any{"kind": "sequenced", "asOfSeq": "5", "values": map[string]any{}},
	}
	for name, projection := range cases {
		t.Run(name, func(t *testing.T) {
			client, calls := newReadTestClient(t, func(endpoint string, args json.RawMessage) any {
				if endpoint == "session/projections" {
					return projection
				}
				t.Errorf("历史不可读时不应请求 %s", endpoint)
				return map[string]any{}
			})
			_, err := client.Read(context.Background(), "session-1")
			if !errors.Is(err, errHistoryUnavailable) {
				t.Fatalf("不可读投影应返回 errHistoryUnavailable，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "历史暂不可读") {
				t.Errorf("错误信息应说明历史暂不可读: %v", err)
			}
			// 读取投影本身是必要请求；历史不可读时不得继续请求 session/page。
			nextReadCall(t, calls, "session/projections")
			select {
			case call := <-calls:
				t.Fatalf("历史不可读时不应继续请求，实际 %s", call.endpoint)
			default:
			}
		})
	}
}

// utf16Length 返回字符串按 UTF-16 代码单元计的长度。
func utf16Length(text string) int {
	return len(utf16.Encode([]rune(text)))
}
