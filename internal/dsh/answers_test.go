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
	"time"
)

// answerRPCCall 保存一次回答相关 RPC 的路径与 args。
type answerRPCCall struct {
	path string
	args json.RawMessage
}

// newAnswerClient 构造捕获 $events/result 与 userQuestions/answer 的测试客户端。
func newAnswerClient(t *testing.T, value any, ok bool) (*Client, <-chan answerRPCCall) {
	t.Helper()
	captured := make(chan answerRPCCall, 4)
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
		captured <- answerRPCCall{path: r.URL.Path, args: request.Payload.Args}

		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = fmt.Fprintf(w,
				`{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"denied","message":"rejected"}}}`,
				request.RPCID,
			)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": request.RPCID,
			"result": map[string]any{
				"ok":    true,
				"value": value,
			},
		})
	})
	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client
	return client, captured
}

// optionQuestion 构造带选项的问题。
func optionQuestion(kind string, values ...string) Question {
	options := make([]Option, 0, len(values))
	for _, value := range values {
		options = append(options, Option{Value: value, Label: value})
	}
	return Question{ID: "q1", Title: "问题", Kind: kind, Options: options, Required: true}
}

func TestParseAnswerNumbersAndCustomText(t *testing.T) {
	t.Run("单选编号加自定义", func(t *testing.T) {
		answer, err := parseAnswer([]int{2}, " 补充说明 ", optionQuestion("single", "A", "B"))
		if err != nil {
			t.Fatalf("解析回答: %v", err)
		}
		if len(answer.Selected) != 1 || answer.Selected[0] != "B" || answer.Custom != "补充说明" {
			t.Errorf("回答 = %+v，期望 B + 自定义文本", answer)
		}
	})

	t.Run("多选多个编号", func(t *testing.T) {
		answer, err := parseAnswer([]int{1, 3}, "补充", optionQuestion("multiple", "A", "B", "C"))
		if err != nil {
			t.Fatalf("解析回答: %v", err)
		}
		if len(answer.Selected) != 2 || answer.Selected[0] != "A" || answer.Selected[1] != "C" {
			t.Errorf("回答 = %+v，期望 A,C", answer)
		}
		if answer.Custom != "补充" {
			t.Errorf("自定义文本 = %q", answer.Custom)
		}
	})

	t.Run("纯文本问题不接受编号", func(t *testing.T) {
		answer, err := parseAnswer(nil, "完整文本答案", Question{ID: "q1", Kind: "text", Required: true})
		if err != nil {
			t.Fatalf("解析回答: %v", err)
		}
		if len(answer.Selected) != 0 || answer.Custom != "完整文本答案" {
			t.Errorf("回答 = %+v", answer)
		}
	})
}

func TestParseAnswerRejectsOutOfRangeAndDuplicate(t *testing.T) {
	cases := []struct {
		name     string
		indexes  []int
		text     string
		question Question
	}{
		{name: "越界", indexes: []int{3}, question: optionQuestion("single", "A", "B")},
		{name: "下界越界", indexes: []int{0}, question: optionQuestion("single", "A", "B")},
		{name: "重复编号", indexes: []int{1, 1}, question: optionQuestion("multiple", "A", "B")},
		{name: "单选多个编号", indexes: []int{1, 2}, question: optionQuestion("single", "A", "B")},
		{name: "文本问题带编号", indexes: []int{1}, text: "答案", question: Question{ID: "q1", Kind: "text"}},
		{name: "自定义文本超长", text: strings.Repeat("字", answerCustomMaxRunes+1), question: Question{ID: "q1", Kind: "text"}},
		{name: "未知问题类型", text: "答案", question: Question{ID: "q1", Kind: "unknown"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseAnswer(testCase.indexes, testCase.text, testCase.question); !errors.Is(err, errAnswerInvalid) {
				t.Fatalf("错误 = %v，期望 errAnswerInvalid", err)
			}
		})
	}

	// 达到上限的自定义文本应当接受。
	if _, err := parseAnswer(nil, strings.Repeat("字", answerCustomMaxRunes), Question{ID: "q1", Kind: "text"}); err != nil {
		t.Fatalf("上限长度自定义文本被拒绝: %v", err)
	}
}

func TestApprovalRejectsCustomText(t *testing.T) {
	question := Question{
		ID:       "approval",
		Kind:     "approval",
		Options:  []Option{{Value: "allowed-once", Label: "允许本次"}, {Value: "rejected", Label: "拒绝"}},
		Required: true,
	}
	if _, err := parseAnswer([]int{1}, "自定义", question); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("审批带自定义文本错误 = %v，期望 errAnswerInvalid", err)
	}
	if _, err := parseAnswer([]int{1, 2}, "", question); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("审批多选错误 = %v，期望 errAnswerInvalid", err)
	}
	if _, err := parseAnswer(nil, "", question); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("审批无选择错误 = %v，期望 errAnswerInvalid", err)
	}

	answer, err := parseAnswer([]int{2}, "", question)
	if err != nil {
		t.Fatalf("解析审批回答: %v", err)
	}
	if len(answer.Selected) != 1 || answer.Selected[0] != "rejected" {
		t.Errorf("审批回答 = %+v，期望 rejected", answer)
	}
}

func TestBuildAnswerValueForApprovalAndForm(t *testing.T) {
	approvalDecision := Decision{
		NativeID:  "event:event-1",
		SessionID: decisionTestSession,
		Kind:      "approval",
		Supported: true,
		Blocking:  true,
		Questions: []Question{{
			ID:       "approval",
			Kind:     "approval",
			Options:  []Option{{Value: "allowed-once", Label: "允许本次"}, {Value: "rejected", Label: "拒绝"}},
			Required: true,
		}},
	}
	answerValue, err := buildAnswerValue(approvalDecision, map[string]Answer{
		"approval": {Selected: []string{"allowed-once"}},
	})
	if err != nil {
		t.Fatalf("构造审批答案: %v", err)
	}
	if answerValue != "allowed-once" {
		t.Errorf("审批答案 = %#v，期望 allowed-once 字符串", answerValue)
	}

	if _, err := buildAnswerValue(approvalDecision, map[string]Answer{
		"approval": {Selected: []string{"always"}},
	}); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("非白名单审批答案错误 = %v，期望 errAnswerInvalid", err)
	}
	if _, err := buildAnswerValue(approvalDecision, map[string]Answer{}); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("缺少审批答案错误 = %v，期望 errAnswerInvalid", err)
	}

	formDecision := Decision{
		NativeID:  "event:event-2",
		SessionID: decisionTestSession,
		Kind:      "form",
		Supported: true,
		Questions: []Question{
			optionQuestion("single", "A", "B"),
			{ID: "q2", Title: "补充", Kind: "text", Required: true},
		},
	}
	answerValue, err = buildAnswerValue(formDecision, map[string]Answer{
		"q1": {Selected: []string{"B"}},
		"q2": {Custom: "补充说明"},
	})
	if err != nil {
		t.Fatalf("构造表单答案: %v", err)
	}
	encoded, err := json.Marshal(answerValue)
	if err != nil {
		t.Fatalf("编码表单答案: %v", err)
	}
	var decoded struct {
		Answers []map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解析表单答案 %s: %v", encoded, err)
	}
	if len(decoded.Answers) != 2 {
		t.Fatalf("答案条数 = %d，期望 2: %s", len(decoded.Answers), encoded)
	}
	if decoded.Answers[0]["id"] != "q1" {
		t.Errorf("第一题 id = %v", decoded.Answers[0]["id"])
	}
	selected, ok := decoded.Answers[0]["selected"].([]any)
	if !ok || len(selected) != 1 || selected[0] != "B" {
		t.Errorf("第一题 selected = %#v", decoded.Answers[0]["selected"])
	}
	if _, exists := decoded.Answers[0]["custom"]; exists {
		t.Errorf("空自定义文本不应写入 custom: %s", encoded)
	}
	if decoded.Answers[1]["custom"] != "补充说明" {
		t.Errorf("第二题 custom = %v", decoded.Answers[1]["custom"])
	}
	if selected, ok := decoded.Answers[1]["selected"].([]any); !ok || len(selected) != 0 {
		t.Errorf("文本题 selected 应为空数组: %#v", decoded.Answers[1]["selected"])
	}

	if _, err := buildAnswerValue(formDecision, map[string]Answer{
		"q1": {Selected: []string{"B"}},
	}); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("缺少问题答案错误 = %v，期望 errAnswerInvalid", err)
	}
	if _, err := buildAnswerValue(formDecision, map[string]Answer{
		"q1": {Selected: []string{"B"}},
		"q2": {Custom: "补充"},
		"q3": {Custom: "多余"},
	}); !errors.Is(err, errAnswerInvalid) {
		t.Fatalf("多余答案错误 = %v，期望 errAnswerInvalid", err)
	}
	if _, err := buildAnswerValue(Decision{NativeID: "event:1", Supported: false}, map[string]Answer{}); !errors.Is(err, errDecisionUnsupported) {
		t.Fatalf("不支持决策错误 = %v，期望 errDecisionUnsupported", err)
	}
}

func TestSubmitAnswerUsesEventOrCallEndpoint(t *testing.T) {
	t.Run("审批使用当前clientId并观察cancel", func(t *testing.T) {
		client, captured := newAnswerClient(t, map[string]any{}, true)
		item := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
			approvalPayload("Bash", "tool-1", "原因"))
		decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "stale-client", Items: []PendingItem{item}}, nil)
		if err != nil {
			t.Fatalf("构造审批决策: %v", err)
		}
		decision := decisions[0]

		mux := newMux(client.host, nil)
		mux.clientID = "current-client"
		mux.items[item.EventID] = item
		mux.order = []string{item.EventID}
		mux.canceled[item.EventID] = 1
		client.mux = mux

		receipt, err := client.SubmitAnswer(context.Background(), decision, "allowed-once")
		if err != nil {
			t.Fatalf("提交审批: %v", err)
		}
		if !receipt.Confirmed || receipt.Message == "" {
			t.Errorf("审批回执 = %+v，期望已确认", receipt)
		}

		call := <-captured
		if call.path != "/api/$events/result" {
			t.Errorf("请求路径 = %q，期望 /api/$events/result", call.path)
		}
		var args struct {
			ClientID string `json:"clientId"`
			EventID  string `json:"eventId"`
			Outcome  struct {
				Kind  string `json:"kind"`
				Value any    `json:"value"`
			} `json:"outcome"`
		}
		if err := json.Unmarshal(call.args, &args); err != nil {
			t.Fatalf("解析审批 args %s: %v", call.args, err)
		}
		if args.ClientID != "current-client" {
			t.Errorf("clientId = %q，期望当前连接的 current-client", args.ClientID)
		}
		if args.EventID != "event-1" || args.Outcome.Kind != "result" || args.Outcome.Value != "allowed-once" {
			t.Errorf("审批 args = %s", call.args)
		}
	})

	t.Run("审批未观察到cancel时返回未确认", func(t *testing.T) {
		client, captured := newAnswerClient(t, map[string]any{}, true)
		item := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
			approvalPayload("Bash", "tool-1", "原因"))
		decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
		if err != nil {
			t.Fatalf("构造审批决策: %v", err)
		}
		mux := newMux(client.host, nil)
		mux.clientID = "client-1"
		mux.items[item.EventID] = item
		mux.order = []string{item.EventID}
		mux.cancelWait = 10 * time.Millisecond
		client.mux = mux

		receipt, err := client.SubmitAnswer(context.Background(), decisions[0], "rejected")
		if err != nil {
			t.Fatalf("提交审批: %v", err)
		}
		if receipt.Confirmed || receipt.Message == "" {
			t.Errorf("未观察到 cancel 时应返回未确认: %+v", receipt)
		}
		if call := <-captured; call.path != "/api/$events/result" {
			t.Errorf("请求路径 = %q", call.path)
		}
	})

	t.Run("事件失效时不提交", func(t *testing.T) {
		client, captured := newAnswerClient(t, map[string]any{}, true)
		item := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
			approvalPayload("Bash", "tool-1", "原因"))
		decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
		if err != nil {
			t.Fatalf("构造审批决策: %v", err)
		}
		mux := newMux(client.host, nil)
		mux.clientID = "current-client"
		client.mux = mux

		receipt, err := client.SubmitAnswer(context.Background(), decisions[0], "allowed-once")
		if !errors.Is(err, errDecisionStale) {
			t.Fatalf("错误 = %v，期望 errDecisionStale", err)
		}
		if receipt.Confirmed || receipt.Message == "" {
			t.Errorf("失效决策回执 = %+v", receipt)
		}
		select {
		case call := <-captured:
			t.Fatalf("失效事件不应提交，实际调用 %s", call.path)
		default:
		}
	})

	t.Run("审批拒绝非白名单值", func(t *testing.T) {
		client, captured := newAnswerClient(t, map[string]any{}, true)
		decision := Decision{NativeID: "event:1", SessionID: decisionTestSession, Kind: "approval", Supported: true}
		if _, err := client.SubmitAnswer(context.Background(), decision, "always"); !errors.Is(err, errAnswerInvalid) {
			t.Fatalf("错误 = %v，期望 errAnswerInvalid", err)
		}
		select {
		case call := <-captured:
			t.Fatalf("非法审批值不应提交，实际调用 %s", call.path)
		default:
		}
	})

	t.Run("持续问题提交到userQuestions", func(t *testing.T) {
		client, captured := newAnswerClient(t, true, true)
		decision := Decision{NativeID: "call:call-1", SessionID: decisionTestSession, Kind: "form", Supported: true}
		value := map[string]any{"answers": []any{map[string]any{"id": "q1", "selected": []string{"A"}}}}

		receipt, err := client.SubmitAnswer(context.Background(), decision, value)
		if err != nil {
			t.Fatalf("提交持续问题: %v", err)
		}
		if !receipt.Confirmed || receipt.Message == "" {
			t.Errorf("持续问题回执 = %+v", receipt)
		}
		call := <-captured
		if call.path != "/api/userQuestions/answer" {
			t.Errorf("请求路径 = %q，期望 /api/userQuestions/answer", call.path)
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal(call.args, &args); err != nil {
			t.Fatalf("解析 args %s: %v", call.args, err)
		}
		if string(args["agentId"]) != `"`+decisionTestSession+`"` {
			t.Errorf("agentId = %s", args["agentId"])
		}
		if string(args["callId"]) != `"call-1"` {
			t.Errorf("callId = %s", args["callId"])
		}
		if !strings.Contains(string(args["answer"]), `"answers"`) {
			t.Errorf("answer = %s", args["answer"])
		}
	})

	t.Run("持续问题未被接受时返回未确认", func(t *testing.T) {
		client, _ := newAnswerClient(t, false, true)
		decision := Decision{NativeID: "call:call-1", SessionID: decisionTestSession, Kind: "form", Supported: true}
		receipt, err := client.SubmitAnswer(context.Background(), decision, map[string]any{"answers": []any{}})
		if err != nil {
			t.Fatalf("提交持续问题: %v", err)
		}
		if receipt.Confirmed || receipt.Message == "" {
			t.Errorf("未被接受的回答应返回未确认: %+v", receipt)
		}
	})

	t.Run("持续问题RPC失败返回错误", func(t *testing.T) {
		client, _ := newAnswerClient(t, nil, false)
		decision := Decision{NativeID: "call:call-1", SessionID: decisionTestSession, Kind: "form", Supported: true}
		receipt, err := client.SubmitAnswer(context.Background(), decision, map[string]any{"answers": []any{}})
		if err == nil {
			t.Fatal("RPC 失败应返回错误")
		}
		if receipt.Confirmed || receipt.Message == "" {
			t.Errorf("失败回执 = %+v", receipt)
		}
	})

	t.Run("不支持的决策不提交", func(t *testing.T) {
		client, captured := newAnswerClient(t, true, true)
		decision := Decision{NativeID: "event:1", SessionID: decisionTestSession, Kind: "unsupported", Supported: false}
		if _, err := client.SubmitAnswer(context.Background(), decision, "x"); !errors.Is(err, errDecisionUnsupported) {
			t.Fatalf("错误 = %v，期望 errDecisionUnsupported", err)
		}
		select {
		case call := <-captured:
			t.Fatalf("不支持决策不应提交，实际调用 %s", call.path)
		default:
		}
	})
}
