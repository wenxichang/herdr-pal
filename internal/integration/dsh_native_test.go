package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

// startDSHNativePal 用原生 dsh 配置启动生产 RunRelay，并交给 harness 清理。
func startDSHNativePal(t *testing.T, harness *hpapHarness, token string, host *testkit.DSHHost) *hpapPal {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "dsh-native.json")
	raw := fmt.Sprintf(
		`{"relay":{"url":%q,"key":%q,"skip_verify":true},"herdr":{"disabled":true},"dsh":{"url":%q,"home":%q,"variant":%q},"log":{"level":"debug"}}`,
		harness.relayURL, token, host.URL(), host.Home(), host.Variant(),
	)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &lockedHPAPBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.RunRelay(ctx, app.Options{ConfigPath: configPath, Stderr: logs, Stdout: nil})
	}()
	return &hpapPal{logs: logs, cancel: cancel, done: done}
}

// waitDSHStatus 等待任一 dsh 会话进入指定状态。
func waitDSHStatus(t *testing.T, harness *hpapHarness, want string) adminproto.Session {
	t.Helper()
	var found adminproto.Session
	eventuallyHPAP(t, "dsh 会话状态 "+want, func() bool {
		result, err := harness.admin.ListSessions(context.Background(), adminproto.SessionListParams{})
		if err != nil {
			return false
		}
		for _, item := range result.Items {
			if item.Agent == "dsh" && item.Status == want {
				found = item
				return true
			}
		}
		return false
	})
	return found
}

// waitWeComPush 等待假企业微信收到包含指定文本的主动推送。
func waitWeComPush(t *testing.T, harness *hpapHarness, want string) string {
	t.Helper()
	var content string
	eventuallyHPAP(t, "企业微信推送包含 "+want, func() bool {
		for _, request := range harness.wecom.CompletedRequests("aibot_send_msg") {
			if strings.Contains(request.Content, want) {
				content = request.Content
				return true
			}
		}
		return false
	})
	return content
}

// waitWeComReply 注入一条企业微信文本并等待对应的命令回复。
func waitWeComReply(t *testing.T, harness *hpapHarness, messageID, content string) string {
	t.Helper()
	completedBefore := len(harness.wecom.CompletedRequests("aibot_respond_msg"))
	requestID := harness.wecom.InjectText(t, messageID, "user-a", "single", content)
	responses := harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", completedBefore+1)
	for _, response := range responses {
		if response.RequestID == requestID {
			return response.Content
		}
	}
	t.Fatalf("企业微信请求 %s 没有对应回复", requestID)
	return ""
}

func (pal *hpapPal) running() bool {
	select {
	case <-pal.done:
		return false
	default:
		return true
	}
}

// dshPromptArgs 是 session/prompt 断言所需的字段。
type dshPromptArgs struct {
	Request struct {
		SessionID string `json:"sessionId"`
		RequestID string `json:"requestId"`
		Mode      string `json:"mode"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"request"`
}

func TestDSHNativeThroughProductionAppAndWeComRouter(t *testing.T) {
	host := testkit.NewDSHHost(t, testkit.DSHHostOptions{
		Sessions: []testkit.DSHSession{{
			ID: "session-history", Title: "历史会话", Cwd: "/work/project", UpdatedAt: 100,
		}},
	})
	host.SetTranscript("session-history", []testkit.DSHEvent{
		testkit.UserMessage(1, "你好"),
		testkit.InjectedUserMessage(2, "INTERNAL_INSTRUCTIONS"),
		testkit.AssistantMessage(3, testkit.ReasoningBlock("HIDDEN_REASONING"), testkit.TextBlock("历史回答")),
		testkit.AssistantMessage(4, testkit.ToolCallBlock("TOOL_CALL_BODY")),
	})
	harness := newHPAPHarness(t)
	issued := harness.issueKey(t, "user-a", "dsh-machine", []string{"127.0.0.1"})
	harness.tokens = append(harness.tokens, issued.Token)
	pal := startDSHNativePal(t, harness, issued.Token, host)
	harness.pals = append(harness.pals, pal)
	harness.waitConnections(t, 1)
	harness.waitSessions(t, 1)

	listReply := waitWeComReply(t, harness, "dsh-list", "/ls")
	for _, want := range []string{"DeepSeek Harness", "历史会话", "project/web"} {
		if !strings.Contains(listReply, want) {
			t.Fatalf("/ls 回复 %q 缺少 %q", listReply, want)
		}
	}

	selectReply := waitWeComReply(t, harness, "dsh-select", "/1")
	for _, want := range []string{"用户：你好", "助手：历史回答"} {
		if !strings.Contains(selectReply, want) {
			t.Fatalf("/1 回复 %q 缺少 %q", selectReply, want)
		}
	}
	for _, forbidden := range []string{"INTERNAL_INSTRUCTIONS", "HIDDEN_REASONING", "TOOL_CALL_BODY"} {
		if strings.Contains(selectReply, forbidden) {
			t.Fatalf("/1 回复泄露内部内容 %q：%s", forbidden, selectReply)
		}
	}

	contentReply := waitWeComReply(t, harness, "dsh-con", "/con")
	if !strings.Contains(contentReply, "用户：你好") || strings.Contains(contentReply, "HIDDEN_REASONING") {
		t.Fatalf("/con 回复 = %q", contentReply)
	}

	// 同一企业微信 msgid 重复投递只能产生一次 dsh prompt；后续 /ls 作为串行屏障。
	promptReply := waitWeComReply(t, harness, "dsh-prompt", "继续处理")
	if !strings.Contains(promptReply, "已发送给 dsh（排队）") {
		t.Fatalf("prompt 回复 = %q", promptReply)
	}
	harness.wecom.InjectText(t, "dsh-prompt", "user-a", "single", "继续处理")
	waitWeComReply(t, harness, "dsh-prompt-barrier", "/ls")
	prompts := host.CallsFor("session/prompt")
	if len(prompts) != 1 {
		t.Fatalf("session/prompt 调用 = %d，期望去重为 1", len(prompts))
	}
	var promptArgs dshPromptArgs
	if err := json.Unmarshal(prompts[0].Args, &promptArgs); err != nil {
		t.Fatalf("解析 prompt 参数：%v", err)
	}
	if promptArgs.Request.SessionID != "session-history" || promptArgs.Request.RequestID != "dsh-prompt" ||
		promptArgs.Request.Mode != "queue" || len(promptArgs.Request.Content) != 1 ||
		promptArgs.Request.Content[0].Text != "继续处理" {
		t.Fatalf("prompt 参数 = %#v", promptArgs)
	}

	host.SetRunning("session-history", true)
	waitDSHStatus(t, harness, "working")
	host.AppendTranscript("session-history", testkit.AssistantMessage(5, testkit.TextBlock("已完成：继续处理")))
	host.SetRunning("session-history", false)
	completion := waitWeComPush(t, harness, "已完成：继续处理")
	if strings.Contains(completion, "HIDDEN_REASONING") || strings.Contains(completion, "INTERNAL_INSTRUCTIONS") {
		t.Fatalf("完成推送泄露内部内容：%s", completion)
	}
}

func TestDSHNativeApprovalAndNewSession(t *testing.T) {
	host := testkit.NewDSHHost(t, testkit.DSHHostOptions{})
	harness := newHPAPHarness(t)
	issued := harness.issueKey(t, "user-a", "dsh-machine", []string{"127.0.0.1"})
	harness.tokens = append(harness.tokens, issued.Token)
	pal := startDSHNativePal(t, harness, issued.Token, host)
	harness.pals = append(harness.pals, pal)
	harness.waitConnections(t, 1)
	harness.waitSessions(t, 1)

	// 没有可控制会话时只暴露宿主入口；新建会话后入口必须消失。
	entryReply := waitWeComReply(t, harness, "dsh-entry-list", "/ls")
	for _, want := range []string{"新建会话", "DeepSeek Harness", "web/dsh-host"} {
		if !strings.Contains(entryReply, want) {
			t.Fatalf("宿主入口 /ls 回复 %q 缺少 %q", entryReply, want)
		}
	}
	waitWeComReply(t, harness, "dsh-entry-select", "/1")

	newReply := waitWeComReply(t, harness, "dsh-new-session", "/new-session 新任务 /tmp")
	for _, want := range []string{"已在 /tmp 新建 dsh 会话「新任务」", "已切换到新会话"} {
		if !strings.Contains(newReply, want) {
			t.Fatalf("/new-session 回复 %q 缺少 %q", newReply, want)
		}
	}
	created, ok := host.LastCreatedSession()
	if !ok || created.Title != "新任务" || created.Cwd != "/tmp" {
		t.Fatalf("新建会话 = %#v, %v", created, ok)
	}
	createCalls := host.WaitCallCount(t, "session/create", 1)
	var createArgs struct {
		Request struct {
			Cwd string `json:"cwd"`
		} `json:"request"`
	}
	if err := json.Unmarshal(createCalls[0].Args, &createArgs); err != nil || createArgs.Request.Cwd != "/tmp" {
		t.Fatalf("session/create 参数 = %s", createCalls[0].Args)
	}
	renameCalls := host.WaitCallCount(t, "session/rename", 1)
	var renameArgs struct {
		Request struct {
			SessionID string `json:"sessionId"`
			Title     string `json:"title"`
		} `json:"request"`
	}
	if err := json.Unmarshal(renameCalls[0].Args, &renameArgs); err != nil ||
		renameArgs.Request.SessionID != created.ID || renameArgs.Request.Title != "新任务" {
		t.Fatalf("session/rename 参数 = %s", renameCalls[0].Args)
	}

	afterReply := waitWeComReply(t, harness, "dsh-after-new", "/ls")
	if !strings.Contains(afterReply, "新任务") || !strings.Contains(afterReply, "（当前选择）") ||
		strings.Contains(afterReply, "新建会话") {
		t.Fatalf("新建后的 /ls 回复 = %q", afterReply)
	}

	// 审批：blocked 内容必须包含工具名、原因和 /answer 提示；只提交 allowed-once。
	host.InjectDecision(t, testkit.ApprovalDecision("event-approval", created.ID, "bash", "运行测试命令"))
	waitDSHStatus(t, harness, "blocked")
	blocked := waitWeComPush(t, harness, "运行测试命令")
	for _, want := range []string{"审批：bash", "运行测试命令", "/answer 1"} {
		if !strings.Contains(blocked, want) {
			t.Fatalf("阻塞推送 %q 缺少 %q", blocked, want)
		}
	}
	answerReply := waitWeComReply(t, harness, "dsh-answer-approval", "/answer 1")
	if !strings.Contains(answerReply, "已提交") {
		t.Fatalf("审批回复 = %q", answerReply)
	}
	eventCalls := host.WaitCallCount(t, "$events/result", 1)
	assertEventAnswer(t, eventCalls[0], host.ReadyClientID(), "event-approval", "allowed-once")

	// 多题：逐题收集，最后一题后一次性提交。
	waitDSHStatus(t, harness, "idle")
	host.InjectDecision(t, testkit.UserQuestionsDecision("event-form", created.ID,
		testkit.DSHQuestion{
			ID: "first", Question: "选择第一项",
			Options: []testkit.DSHOption{{Label: "选项甲"}, {Label: "选项乙"}},
		},
		testkit.DSHQuestion{ID: "second", Question: "补充说明"},
	))
	waitDSHStatus(t, harness, "blocked")
	firstReply := waitWeComReply(t, harness, "dsh-answer-form-1", "/answer 1")
	if !strings.Contains(firstReply, "已记录第 1/2 题答案") || !strings.Contains(firstReply, "补充说明") {
		t.Fatalf("多题第一题回复 = %q", firstReply)
	}
	if calls := host.CallsFor("$events/result"); len(calls) != 1 {
		t.Fatalf("多题未完成前不应提交，$events/result 调用 = %d", len(calls))
	}
	secondReply := waitWeComReply(t, harness, "dsh-answer-form-2", "/answer 自定义答案")
	if !strings.Contains(secondReply, "已提交") {
		t.Fatalf("多题提交回复 = %q", secondReply)
	}
	eventCalls = host.WaitCallCount(t, "$events/result", 2)
	assertFormAnswer(t, eventCalls[1], host.ReadyClientID(), "event-form")
}

func assertEventAnswer(t *testing.T, call testkit.DSHCall, clientID, eventID, wantValue string) {
	t.Helper()
	var args struct {
		ClientID string `json:"clientId"`
		EventID  string `json:"eventId"`
		Outcome  struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("解析 $events/result 参数：%v", err)
	}
	if args.ClientID != clientID || args.EventID != eventID || args.Outcome.Kind != "result" ||
		args.Outcome.Value != wantValue {
		t.Fatalf("$events/result 参数 = %s", call.Args)
	}
}

func assertFormAnswer(t *testing.T, call testkit.DSHCall, clientID, eventID string) {
	t.Helper()
	var args struct {
		ClientID string `json:"clientId"`
		EventID  string `json:"eventId"`
		Outcome  struct {
			Kind  string `json:"kind"`
			Value struct {
				Answers []struct {
					ID       string   `json:"id"`
					Selected []string `json:"selected"`
					Custom   string   `json:"custom"`
				} `json:"answers"`
			} `json:"value"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("解析多题答案：%v", err)
	}
	if args.ClientID != clientID || args.EventID != eventID || args.Outcome.Kind != "result" ||
		len(args.Outcome.Value.Answers) != 2 {
		t.Fatalf("多题答案参数 = %s", call.Args)
	}
	first, second := args.Outcome.Value.Answers[0], args.Outcome.Value.Answers[1]
	if first.ID != "first" || len(first.Selected) != 1 || first.Selected[0] != "选项甲" || first.Custom != "" {
		t.Fatalf("第一题答案 = %#v", first)
	}
	if second.ID != "second" || len(second.Selected) != 0 || second.Custom != "自定义答案" {
		t.Fatalf("第二题答案 = %#v", second)
	}
}

func TestDSHNativeHostDisappearanceWithdrawsTargets(t *testing.T) {
	host := testkit.NewDSHHost(t, testkit.DSHHostOptions{
		Sessions: []testkit.DSHSession{{ID: "vanishing", Title: "消失会话", Cwd: "/work/gone"}},
	})
	harness := newHPAPHarness(t)
	issued := harness.issueKey(t, "user-a", "dsh-machine", []string{"127.0.0.1"})
	harness.tokens = append(harness.tokens, issued.Token)
	pal := startDSHNativePal(t, harness, issued.Token, host)
	harness.pals = append(harness.pals, pal)
	harness.waitConnections(t, 1)
	harness.waitSessions(t, 1)

	if reply := waitWeComReply(t, harness, "dsh-before-close", "/ls"); !strings.Contains(reply, "消失会话") {
		t.Fatalf("关闭前 /ls 回复 = %q", reply)
	}
	host.Close()
	harness.waitSessions(t, 0)
	if !pal.running() {
		t.Fatalf("宿主消失后 Pal 不应退出：%s", pal.logs.String())
	}
	if reply := waitWeComReply(t, harness, "dsh-after-close", "/ls"); !strings.Contains(reply, "当前没有可用会话") {
		t.Fatalf("关闭后 /ls 回复 = %q", reply)
	}
	connections, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
	if err != nil || len(connections.Items) != 1 {
		t.Fatalf("宿主消失后 Relay 连接 = %#v, %v", connections.Items, err)
	}
}
