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

const (
	// decisionTestSession 是决策测试使用的 dsh 会话 ID。
	decisionTestSession = "session-1"
	// decisionTestOtherSession 用于验证决策按会话隔离。
	decisionTestOtherSession = "session-2"
)

// pendingWaterfall 构造一条 waterfall 待答事件，并让 Raw 与 Request 保持一致。
func pendingWaterfall(t *testing.T, eventID, agentID, event string, request any) PendingItem {
	t.Helper()
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("编码 waterfall request: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"type":    "waterfall",
		"eventId": eventID,
		"agentId": agentID,
		"event":   event,
		"request": json.RawMessage(requestRaw),
	})
	if err != nil {
		t.Fatalf("编码 waterfall 帧: %v", err)
	}
	return PendingItem{
		EventID: eventID,
		AgentID: agentID,
		Event:   event,
		Request: requestRaw,
		Raw:     raw,
	}
}

// approvalPayload 返回审批瀑布请求的典型载荷。
func approvalPayload(toolName, callID, reason string) map[string]any {
	request := map[string]any{
		"agent":    map[string]any{"id": decisionTestSession},
		"toolName": toolName,
	}
	if callID != "" {
		request["callId"] = callID
	}
	if reason != "" {
		request["reason"] = reason
	}
	return request
}

// questionRequestPayload 返回用户问题瀑布请求的典型载荷。
func questionRequestPayload(callID string, timed bool, questions ...map[string]any) map[string]any {
	request := map[string]any{
		"agent":     map[string]any{"id": decisionTestSession},
		"questions": questions,
	}
	if callID != "" {
		request["wait"] = map[string]any{"callId": callID, "timed": timed}
	}
	return request
}

// projectionsWithActive 构造包含持续问题 active 列表的会话投影。
func projectionsWithActive(t *testing.T, entries ...map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"kind":    "sequenced",
		"asOfSeq": 7,
		"values":  map[string]any{"userQuestions": map[string]any{"active": entries}},
	})
	if err != nil {
		t.Fatalf("编码会话投影: %v", err)
	}
	return raw
}

// continuedEntry 返回一条持续问题投影条目。
func continuedEntry(callID, state string, questions ...map[string]any) map[string]any {
	return map[string]any{
		"callId":    callID,
		"state":     state,
		"questions": questions,
	}
}

func TestDecisionsCombineWaterfallAndContinued(t *testing.T) {
	approval := pendingWaterfall(t, "event-approval", decisionTestSession, "approval/request",
		approvalPayload("Bash", "tool-1", "需要写入文件"))
	timed := pendingWaterfall(t, "event-timed", decisionTestSession, "user-questions/request",
		questionRequestPayload("call-timed", true, map[string]any{
			"id":       "q1",
			"question": "选择方案",
			"options":  []map[string]any{{"label": "方案 A"}, {"label": "方案 B"}},
		}))
	otherSession := pendingWaterfall(t, "event-other", decisionTestOtherSession, "approval/request",
		approvalPayload("Bash", "", "其他会话"))

	projections := projectionsWithActive(t,
		// timed waterfall 已包含同一 callId，投影中的重复项必须被跳过。
		continuedEntry("call-timed", "continued", map[string]any{
			"id": "q1", "question": "选择方案",
			"options": []map[string]any{{"label": "方案 A"}, {"label": "方案 B"}},
		}),
		continuedEntry("call-continued", "continued", map[string]any{
			"id": "q2", "question": "补充说明",
		}),
	)

	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{
		ClientID: "client-1",
		Items:    []PendingItem{approval, timed, otherSession},
	}, projections)
	if err != nil {
		t.Fatalf("解析决策: %v", err)
	}
	if len(decisions) != 3 {
		t.Fatalf("决策数 = %d，期望 3: %+v", len(decisions), decisions)
	}
	wantIDs := []string{"event:event-approval", "event:event-timed", "call:call-continued"}
	for index, want := range wantIDs {
		if decisions[index].NativeID != want {
			t.Errorf("决策 %d NativeID = %q，期望 %q", index, decisions[index].NativeID, want)
		}
		if decisions[index].SessionID != decisionTestSession {
			t.Errorf("决策 %d SessionID = %q，期望 %q", index, decisions[index].SessionID, decisionTestSession)
		}
	}
	if !decisions[0].Blocking || decisions[0].Kind != "approval" {
		t.Errorf("审批决策应为 blocking approval: %+v", decisions[0])
	}
	if decisions[1].Blocking {
		t.Errorf("定时问题不应 blocking: %+v", decisions[1])
	}
	if !decisions[2].Supported || decisions[2].Kind != "form" {
		t.Errorf("continued 投影问题应可答: %+v", decisions[2])
	}
}

func TestApprovalIsBlockingAndOnlyAllowsTwoOutcomes(t *testing.T) {
	item := pendingWaterfall(t, "event-approval", decisionTestSession, "approval/request",
		approvalPayload("Edit", "tool-9", "修改生产配置"))
	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
	if err != nil {
		t.Fatalf("解析审批决策: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("决策数 = %d，期望 1", len(decisions))
	}
	decision := decisions[0]
	if decision.NativeID != "event:event-approval" || decision.Kind != "approval" || !decision.Supported || !decision.Blocking {
		t.Errorf("审批决策字段错误: %+v", decision)
	}
	if len(decision.Questions) != 1 {
		t.Fatalf("审批问题数 = %d，期望 1", len(decision.Questions))
	}
	question := decision.Questions[0]
	if question.Kind != "approval" || !question.Required {
		t.Errorf("审批问题类型/必填错误: %+v", question)
	}
	if len(question.Options) != 2 {
		t.Fatalf("审批选项数 = %d，期望 2", len(question.Options))
	}
	if question.Options[0].Value != "allowed-once" || question.Options[1].Value != "rejected" {
		t.Errorf("审批选项值 = %+v，期望 allowed-once/rejected", question.Options)
	}
	if question.Options[0].Label != "允许本次" || question.Options[1].Label != "拒绝" {
		t.Errorf("审批选项标签 = %+v", question.Options)
	}
	if !strings.Contains(decision.Title, "Edit") {
		t.Errorf("审批标题应包含工具名: %q", decision.Title)
	}
	if question.Description != "修改生产配置" {
		t.Errorf("审批描述 = %q，期望工具原因", question.Description)
	}
}

func TestTimedQuestionIsNotBlocking(t *testing.T) {
	timed := pendingWaterfall(t, "event-timed", decisionTestSession, "user-questions/request",
		questionRequestPayload("call-1", true, map[string]any{"id": "q1", "question": "定时确认"}))
	blocking := pendingWaterfall(t, "event-blocking", decisionTestSession, "user-questions/request",
		questionRequestPayload("call-2", false, map[string]any{"id": "q1", "question": "必须回答"}))

	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{
		ClientID: "client-1",
		Items:    []PendingItem{timed, blocking},
	}, nil)
	if err != nil {
		t.Fatalf("解析问题决策: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("决策数 = %d，期望 2", len(decisions))
	}
	if decisions[0].Blocking {
		t.Errorf("timed=true 的问题不应 blocking: %+v", decisions[0])
	}
	if !decisions[1].Blocking {
		t.Errorf("非 timed 问题应 blocking: %+v", decisions[1])
	}
	for _, decision := range decisions {
		if !decision.Supported || decision.Kind != "form" {
			t.Errorf("问题决策应可答: %+v", decision)
		}
	}
}

func TestUnknownWaterfallIsUnsupported(t *testing.T) {
	item := pendingWaterfall(t, "event-terminal", decisionTestSession, "terminal/open", map[string]any{"command": "ls"})
	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
	if err != nil {
		t.Fatalf("解析未知交互: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("决策数 = %d，期望 1", len(decisions))
	}
	decision := decisions[0]
	if decision.Supported || decision.Blocking || decision.Kind != "unsupported" {
		t.Errorf("未知 waterfall 应 unsupported 且非 blocking: %+v", decision)
	}
	if decision.Reason == "" || decision.NativeID != "event:event-terminal" {
		t.Errorf("未知 waterfall 应保留事实与原因: %+v", decision)
	}
}

func TestContinuedQuestionRequiresContinuedState(t *testing.T) {
	projections := projectionsWithActive(t,
		continuedEntry("call-open", "open", map[string]any{"id": "q1", "question": "仍在等待浏览器"}),
		continuedEntry("call-continued", "continued", map[string]any{"id": "q2", "question": "可远程回答"}),
	)
	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{}, projections)
	if err != nil {
		t.Fatalf("解析持续问题: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("决策数 = %d，期望 2", len(decisions))
	}
	if decisions[0].Supported || decisions[0].Blocking || decisions[0].Reason == "" {
		t.Errorf("state=open 应 unsupported 且非 blocking: %+v", decisions[0])
	}
	if !decisions[1].Supported || decisions[1].NativeID != "call:call-continued" {
		t.Errorf("state=continued 应可答: %+v", decisions[1])
	}
	if decisions[1].Kind != "form" {
		t.Errorf("持续问题 Kind = %q，期望 form", decisions[1].Kind)
	}
}

func TestDecisionRevisionChangesWithFrame(t *testing.T) {
	first := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
		approvalPayload("Bash", "tool-1", "第一次原因"))
	second := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
		approvalPayload("Bash", "tool-1", "第二次原因"))
	repeated := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
		approvalPayload("Bash", "tool-1", "第一次原因"))

	revisionOf := func(item PendingItem) string {
		t.Helper()
		decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
		if err != nil {
			t.Fatalf("解析决策: %v", err)
		}
		if len(decisions) != 1 {
			t.Fatalf("决策数 = %d，期望 1", len(decisions))
		}
		return decisions[0].Revision
	}

	firstRevision := revisionOf(first)
	secondRevision := revisionOf(second)
	repeatedRevision := revisionOf(repeated)
	if len(firstRevision) != 64 {
		t.Errorf("revision 长度 = %d，期望 sha256 十六进制 64", len(firstRevision))
	}
	if firstRevision == secondRevision {
		t.Errorf("帧内容变化后 revision 不应相同: %q", firstRevision)
	}
	if firstRevision != repeatedRevision {
		t.Errorf("相同帧应得到相同 revision: %q != %q", firstRevision, repeatedRevision)
	}

	// 投影条目同样以条目自身内容计算 revision。
	firstProjection := projectionsWithActive(t, continuedEntry("call-1", "continued", map[string]any{"id": "q1", "question": "问题 A"}))
	secondProjection := projectionsWithActive(t, continuedEntry("call-1", "continued", map[string]any{"id": "q1", "question": "问题 B"}))
	projectionRevision := func(raw json.RawMessage) string {
		t.Helper()
		decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{}, raw)
		if err != nil {
			t.Fatalf("解析投影: %v", err)
		}
		if len(decisions) != 1 {
			t.Fatalf("决策数 = %d，期望 1", len(decisions))
		}
		return decisions[0].Revision
	}
	if projectionRevision(firstProjection) == projectionRevision(secondProjection) {
		t.Error("投影问题内容变化后 revision 不应相同")
	}
}

func TestDecisionsRejectMalformedQuestions(t *testing.T) {
	question := func(id string, options ...map[string]any) map[string]any {
		return map[string]any{"id": id, "question": "问题", "options": options}
	}
	manyQuestions := make([]map[string]any, 0, decisionMaxQuestions+1)
	for index := 0; index <= decisionMaxQuestions; index++ {
		manyQuestions = append(manyQuestions, question(fmt.Sprintf("q%d", index)))
	}

	cases := []struct {
		name      string
		questions []map[string]any
	}{
		{name: "没有问题", questions: nil},
		{name: "问题过多", questions: manyQuestions},
		{name: "问题ID重复", questions: []map[string]any{question("q1"), question("q1")}},
		{name: "选项标签为空", questions: []map[string]any{question("q1", map[string]any{"label": ""})}},
		{name: "问题文本过长", questions: []map[string]any{{
			"id":       "q1",
			"question": strings.Repeat("x", decisionTextMaxBytes+1),
		}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := pendingWaterfall(t, "event-1", decisionTestSession, "user-questions/request",
				questionRequestPayload("", false, testCase.questions...))
			_, err := decisionsFrom(decisionTestSession, PendingSnapshot{ClientID: "client-1", Items: []PendingItem{item}}, nil)
			if !errors.Is(err, errDecisionMalformed) {
				t.Fatalf("错误 = %v，期望 errDecisionMalformed", err)
			}
		})
	}
}

func TestFormatDecisionRendersOptionsAndProgress(t *testing.T) {
	decision := Decision{
		NativeID:  "event:event-1",
		SessionID: decisionTestSession,
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Supported: true,
		Questions: []Question{
			{
				ID:      "q1",
				Title:   "选择方案",
				Kind:    questionKindSingle,
				Options: []Option{{Value: "A", Label: "方案 A", Description: "推荐"}},
			},
			{ID: "q2", Title: "补充说明", Kind: questionKindText},
		},
	}
	first := formatDecision(decision, 0)
	for _, want := range []string{"dsh 提问", "(1/2)", "选择方案", "1. 方案 A", "（推荐）", "/answer 1"} {
		if !strings.Contains(first, want) {
			t.Errorf("第一题渲染缺少 %q:\n%s", want, first)
		}
	}
	second := formatDecision(decision, 1)
	for _, want := range []string{"(2/2)", "补充说明", "/answer 自定义文本"} {
		if !strings.Contains(second, want) {
			t.Errorf("第二题渲染缺少 %q:\n%s", want, second)
		}
	}

	approval := Decision{
		NativeID:  "event:event-2",
		SessionID: decisionTestSession,
		Kind:      decisionKindApproval,
		Title:     "审批：Bash",
		Supported: true,
		Questions: []Question{{
			ID:      "approval",
			Title:   "是否允许本次工具调用？",
			Kind:    questionKindApproval,
			Options: approvalOptions(),
		}},
	}
	rendered := formatDecision(approval, 0)
	for _, want := range []string{"1. 允许本次", "2. 拒绝", "/answer 1", "/answer 2"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("审批渲染缺少 %q:\n%s", want, rendered)
		}
	}

	unsupported := Decision{
		NativeID:  "event:event-3",
		SessionID: decisionTestSession,
		Kind:      decisionKindUnsupported,
		Title:     "dsh 交互",
		Reason:    "不支持的 dsh 交互 \"terminal/open\"",
	}
	rendered = formatDecision(unsupported, 0)
	if !strings.Contains(rendered, "不支持") || !strings.Contains(rendered, "dsh 原生界面") {
		t.Errorf("不支持决策渲染缺少说明:\n%s", rendered)
	}
}

func TestDecisionsWithoutSourcesAreEmpty(t *testing.T) {
	decisions, err := decisionsFrom(decisionTestSession, PendingSnapshot{}, nil)
	if err != nil {
		t.Fatalf("空来源解析失败: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("空来源决策数 = %d，期望 0", len(decisions))
	}
	// 投影返回 null 表示会话不存在，不应视为协议错误。
	decisions, err = decisionsFrom(decisionTestSession, PendingSnapshot{}, json.RawMessage("null"))
	if err != nil {
		t.Fatalf("null 投影解析失败: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("null 投影决策数 = %d，期望 0", len(decisions))
	}
}

// newProjectionClient 构造只响应 session/projections 的测试客户端。
func newProjectionClient(t *testing.T, value any, fail bool) *Client {
	t.Helper()
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		rpcID := readRPCID(t, r)
		w.Header().Set("Content-Type", "application/json")
		if fail {
			_, _ = fmt.Fprintf(w,
				`{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"unavailable","message":"projection failed"}}}`,
				rpcID,
			)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": rpcID,
			"result": map[string]any{
				"ok":    true,
				"value": value,
			},
		})
	})
	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client
	return client
}

// attachPendingItems 把待答项直接放入客户端的 Mux 快照，供不启动订阅的测试使用。
func attachPendingItems(t *testing.T, client *Client, clientID string, items ...PendingItem) {
	t.Helper()
	mux := newMux(client.host, nil)
	mux.clientID = clientID
	for _, item := range items {
		mux.items[item.EventID] = item
		mux.order = append(mux.order, item.EventID)
	}
	client.mux = mux
}

func TestClientDecisionsMergesAndDegradesSources(t *testing.T) {
	projection := map[string]any{
		"kind":    "sequenced",
		"asOfSeq": 3,
		"values": map[string]any{"userQuestions": map[string]any{"active": []any{
			continuedEntry("call-1", "continued", map[string]any{"id": "q1", "question": "投影问题"}),
		}}},
	}
	approval := pendingWaterfall(t, "event-1", decisionTestSession, "approval/request",
		approvalPayload("Bash", "tool-1", "原因"))

	t.Run("Mux不可用时返回投影问题", func(t *testing.T) {
		client := newProjectionClient(t, projection, false)
		decisions, err := client.Decisions(context.Background(), decisionTestSession)
		if err != nil {
			t.Fatalf("读取决策: %v", err)
		}
		if len(decisions) != 1 || decisions[0].NativeID != "call:call-1" {
			t.Fatalf("决策 = %+v，期望投影中的持续问题", decisions)
		}
	})

	t.Run("投影不可用时返回Mux审批", func(t *testing.T) {
		client := newProjectionClient(t, nil, true)
		attachPendingItems(t, client, "client-1", approval)
		decisions, err := client.Decisions(context.Background(), decisionTestSession)
		if err != nil {
			t.Fatalf("读取决策: %v", err)
		}
		if len(decisions) != 1 || decisions[0].NativeID != "event:event-1" {
			t.Fatalf("决策 = %+v，期望 Mux 审批", decisions)
		}
	})

	t.Run("投影格式错误时降级到Mux", func(t *testing.T) {
		client := newProjectionClient(t, []any{}, false)
		attachPendingItems(t, client, "client-1", approval)
		decisions, err := client.Decisions(context.Background(), decisionTestSession)
		if err != nil {
			t.Fatalf("读取决策: %v", err)
		}
		if len(decisions) != 1 || decisions[0].NativeID != "event:event-1" {
			t.Fatalf("决策 = %+v，期望 Mux 审批", decisions)
		}
	})

	t.Run("Mux格式错误时降级到投影", func(t *testing.T) {
		client := newProjectionClient(t, projection, false)
		broken := PendingItem{EventID: "event-broken", AgentID: decisionTestSession, Event: "approval/request", Raw: json.RawMessage("not-json")}
		attachPendingItems(t, client, "client-1", broken)
		decisions, err := client.Decisions(context.Background(), decisionTestSession)
		if err != nil {
			t.Fatalf("读取决策: %v", err)
		}
		if len(decisions) != 1 || decisions[0].NativeID != "call:call-1" {
			t.Fatalf("决策 = %+v，期望投影中的持续问题", decisions)
		}
	})

	t.Run("两个来源都失败时返回错误", func(t *testing.T) {
		client := newProjectionClient(t, nil, true)
		if _, err := client.Decisions(context.Background(), decisionTestSession); err == nil {
			t.Fatal("两个来源都不可用时应返回错误")
		}
	})

	t.Run("拒绝非法会话ID", func(t *testing.T) {
		client := newProjectionClient(t, projection, false)
		if _, err := client.Decisions(context.Background(), ""); err == nil {
			t.Fatal("空会话 ID 应被拒绝")
		}
	})
}
