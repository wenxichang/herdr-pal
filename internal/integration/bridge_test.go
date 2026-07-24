package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

const (
	testBotID  = "bot-integration"
	testSecret = "secret-integration"
	testUserID = "user-integration"
)

func TestBridgeEndToEnd(t *testing.T) {
	t.Run("启动后完成协议门禁快照与两侧订阅", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)

		harness.herdr.WaitSubscription(t, []herdr.SubscriptionSpec{
			{Type: "pane.created"},
			{Type: "pane.closed"},
			{Type: "pane.updated"},
			{Type: "pane.exited"},
			{Type: "pane.agent_detected"},
		})
		harness.herdr.WaitSubscription(t, []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}})

		if err := harness.ctx.Err(); err != nil {
			t.Fatalf("应用在启动基线期间已停止：%v", err)
		}
	})

	t.Run("列表选择与普通文本仅发送一次 prompt", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusIdle)
		defer harness.stop(t)

		harness.send(t, "message-list", testUserID, "single", "/ls")
		harness.send(t, "message-select", testUserID, "single", "/1")
		harness.send(t, "message-prompt", testUserID, "single", "继续实现端到端测试")

		calls := harness.herdr.WaitCallCount(t, "agent.prompt", 1)
		assertCallParams(t, calls[0], map[string]any{
			"target": "pane-1",
			"text":   "继续实现端到端测试",
			"wait": map[string]any{
				"until": []any{"idle", "working", "blocked", "done", "unknown"},
			},
		})
		getCalls := harness.herdr.WaitCallCount(t, "agent.get", 1)
		assertCallParams(t, getCalls[0], map[string]any{"target": "pane-1"})
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.prompt")) }, 1)
	})

	t.Run("prompt 未触发状态变化时补发一次 enter", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusIdle)
		defer harness.stop(t)
		harness.herdr.SetPromptWaitStalls(1)
		harness.herdr.SetEnterTransition(herdr.AgentStatusWorking)
		harness.selectFirst(t)

		reply := harness.send(t, "message-prompt-recovery", testUserID, "single", "继续执行")
		if !strings.Contains(reply.Content, "working") {
			t.Fatalf("prompt 恢复回复 = %q", reply.Content)
		}
		promptCalls := harness.herdr.WaitCallCount(t, "agent.prompt", 1)
		assertCallParams(t, promptCalls[0], map[string]any{
			"target": "pane-1",
			"text":   "继续执行",
		})
		keyCalls := harness.herdr.WaitCallCount(t, "agent.send_keys", 1)
		assertCallParams(t, keyCalls[0], map[string]any{"target": "pane-1", "keys": []any{"enter"}})
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.prompt")) }, 1)
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.send_keys")) }, 1)
	})

	t.Run("working 状态拒绝普通文本", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)
		harness.selectFirst(t)

		reply := harness.send(t, "message-working-prompt", testUserID, "single", "不得发送")
		if !strings.Contains(reply.Content, "working") {
			t.Fatalf("working 拒绝回复 = %q", reply.Content)
		}
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.prompt")) }, 0)
	})

	t.Run("显式 enter 与连续按键逐键发送并各刷新一次控制台", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusBlocked)
		defer harness.stop(t)
		harness.selectFirst(t)
		harness.herdr.SetOutput([]string{"key-console"})

		enterReply := harness.send(t, "message-enter", testUserID, "single", "/key enter")
		sequenceReply := harness.send(t, "message-sequence", testUserID, "single", "/key down,sp")
		if !strings.Contains(enterReply.Content, "1/1") || !strings.Contains(enterReply.Content, "key-console") {
			t.Fatalf("enter 回复 = %q", enterReply.Content)
		}
		if !strings.Contains(sequenceReply.Content, "2/2") || !strings.Contains(sequenceReply.Content, "key-console") {
			t.Fatalf("连续按键回复 = %q", sequenceReply.Content)
		}

		calls := harness.herdr.WaitCallCount(t, "agent.send_keys", 3)
		assertCallParams(t, calls[0], map[string]any{"target": "pane-1", "keys": []any{"enter"}})
		assertCallParams(t, calls[1], map[string]any{"target": "pane-1", "keys": []any{"down"}})
		assertCallParams(t, calls[2], map[string]any{"target": "pane-1", "keys": []any{"space"}})
		getCalls := harness.herdr.WaitCallCount(t, "agent.get", 3)
		for _, call := range getCalls {
			assertCallParams(t, call, map[string]any{"target": "pane-1"})
		}
		readCalls := harness.herdr.WaitCallCount(t, "agent.read", 2)
		for _, call := range readCalls {
			assertCallParams(t, call, map[string]any{"target": "pane-1", "lines": float64(100)})
		}
	})

	t.Run("终端内容按一百行分页且向下翻页不读取 Herdr", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)
		harness.selectFirst(t)
		harness.herdr.SetOutput(numberedLines(250))

		latest := harness.send(t, "message-content", testUserID, "single", "/con")
		if !strings.Contains(latest.Content, "line-151") || !strings.Contains(latest.Content, "line-250") || strings.Contains(latest.Content, "line-150") {
			t.Fatalf("/con 回复不代表最后 100 行：%q", latest.Content)
		}
		older := harness.send(t, "message-pageup", testUserID, "single", "/pageup")
		if !strings.Contains(older.Content, "line-051") || !strings.Contains(older.Content, "line-150") || strings.Contains(older.Content, "line-151") {
			t.Fatalf("/pageup 回复不代表较早 100 行：%q", older.Content)
		}
		newer := harness.send(t, "message-pagedn", testUserID, "single", "/pagedn")
		if !strings.Contains(newer.Content, "line-151") || !strings.Contains(newer.Content, "line-250") {
			t.Fatalf("/pagedn 回复未返回缓存中的最新页：%q", newer.Content)
		}

		calls := harness.herdr.Calls("agent.read")
		if len(calls) != 2 {
			t.Fatalf("agent.read 调用数 = %d, want 2", len(calls))
		}
		assertCallParams(t, calls[0], map[string]any{"target": "pane-1", "lines": float64(100)})
		assertCallParams(t, calls[1], map[string]any{"target": "pane-1", "lines": float64(200)})
	})

	t.Run("blocked 与 done 自动通知每次只读最近一百行", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)
		harness.herdr.SetOutput(numberedLines(180))
		agent := "codex"

		if delivered := harness.herdr.EmitStatus(herdr.AgentStatusEvent{
			PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusBlocked, Agent: &agent,
		}); delivered != 1 {
			t.Fatalf("blocked 事件写入订阅数 = %d, want 1", delivered)
		}
		harness.herdr.WaitCallCount(t, "agent.read", 1)
		blockedMessages := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 2)
		assertRecentNotification(t, blockedMessages[:2])
		if delivered := harness.herdr.EmitStatus(herdr.AgentStatusEvent{
			PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusDone, Agent: &agent,
		}); delivered != 1 {
			t.Fatalf("done 事件写入订阅数 = %d, want 1", delivered)
		}
		calls := harness.herdr.WaitCallCount(t, "agent.read", 2)
		doneMessages := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 4)
		assertRecentNotification(t, doneMessages[2:4])
		for _, call := range calls {
			assertCallParams(t, call, map[string]any{"lines": float64(100)})
		}
	})

	t.Run("重复 msgid 不重复 prompt 或按键", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusIdle)
		defer harness.stop(t)
		harness.selectFirst(t)

		harness.send(t, "duplicate-prompt", testUserID, "single", "只执行一次")
		harness.wecom.InjectText(t, "duplicate-prompt", testUserID, "single", "只执行一次")
		harness.send(t, "duplicate-key", testUserID, "single", "/enter")
		harness.wecom.InjectText(t, "duplicate-key", testUserID, "single", "/enter")

		harness.herdr.WaitCallCount(t, "agent.prompt", 1)
		harness.herdr.WaitCallCount(t, "agent.send_keys", 1)
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.prompt")) }, 1)
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.send_keys")) }, 1)
	})

	t.Run("未授权用户与群聊不会产生 Herdr 输入", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)
		harness.selectFirst(t)

		harness.wecom.InjectText(t, "unauthorized", "other-user", "single", "禁止输入")
		harness.wecom.InjectText(t, "group-message", testUserID, "group", "/enter")
		assertStableCount(t, func() int {
			return len(harness.herdr.Calls("agent.prompt")) + len(harness.herdr.Calls("agent.send_keys"))
		}, 0)
	})

	t.Run("occupant 替换使旧选择失效", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusWorking)
		defer harness.stop(t)
		harness.selectFirst(t)
		baselineSnapshots := len(harness.herdr.Calls("session.snapshot"))

		harness.herdr.SetSnapshot(integrationSnapshot("session-2", herdr.AgentStatusWorking))
		harness.herdr.EmitLifecycle("pane.updated", "pane-1")
		harness.herdr.WaitCallCount(t, "session.snapshot", baselineSnapshots+2)
		harness.wecom.WaitRequestCount(t, "aibot_send_msg", 1)
		reply := harness.send(t, "message-after-replace", testUserID, "single", "不得发送")

		if !strings.Contains(reply.Content, "/ls") || !strings.Contains(reply.Content, "/sel") {
			t.Fatalf("occupant 替换后的回复不可操作：%q", reply.Content)
		}
		if calls := harness.herdr.Calls("agent.prompt"); len(calls) != 0 {
			t.Fatalf("occupant 替换后仍发送 prompt：%#v", calls)
		}
	})

	t.Run("Herdr 断线期间暂停输入且重连后必须重新选择", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusIdle)
		defer harness.stop(t)
		harness.selectFirst(t)

		harness.herdr.SetAvailable(false)
		harness.herdr.DisconnectSubscriptions()
		harness.herdr.WaitCallCount(t, "ping", 2)
		degraded := harness.send(t, "message-degraded", testUserID, "single", "断线期间不得发送")
		if !strings.Contains(degraded.Content, "暂不可用") || !strings.Contains(degraded.Content, "未执行") {
			t.Fatalf("Herdr degraded 回复 = %q", degraded.Content)
		}
		if calls := harness.herdr.Calls("agent.prompt"); len(calls) != 0 {
			t.Fatalf("Herdr 断线期间仍发送 prompt：%#v", calls)
		}

		lifecycleSpecs := herdr.LifecycleSubscriptions()
		statusSpecs := []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}}
		sentBeforeReconnect := len(harness.wecom.Requests("aibot_send_msg"))
		harness.herdr.SetAvailable(true)
		harness.herdr.WaitCallCount(t, "ping", 3)
		harness.herdr.WaitSubscriptionCount(t, lifecycleSpecs, 2)
		harness.herdr.WaitSubscriptionCount(t, statusSpecs, 2)
		harness.herdr.WaitCallCount(t, "session.snapshot", 4)
		beforeProbe := len(harness.herdr.Calls("session.snapshot"))
		if delivered := harness.herdr.EmitLifecycle("pane.updated", "pane-1"); delivered != 1 {
			t.Fatalf("重连后的 lifecycle 投递数 = %d, want 1", delivered)
		}
		harness.herdr.WaitCallCount(t, "session.snapshot", beforeProbe+1)
		assertStableCount(t, func() int { return len(harness.wecom.Requests("aibot_send_msg")) }, sentBeforeReconnect)

		reselect := harness.send(t, "message-reconnected-select", testUserID, "single", "/sel 1")
		if !strings.Contains(reselect.Content, "先执行 /ls") {
			t.Fatalf("Herdr 重连后旧列表编号仍可用：%q", reselect.Content)
		}
		withoutSelection := harness.send(t, "message-reconnected-prompt", testUserID, "single", "重连后仍不得沿用选择")
		if !strings.Contains(withoutSelection.Content, "/ls") || !strings.Contains(withoutSelection.Content, "/sel") {
			t.Fatalf("Herdr 重连后的普通文本回复 = %q", withoutSelection.Content)
		}
		if calls := harness.herdr.Calls("agent.prompt"); len(calls) != 0 {
			t.Fatalf("Herdr 重连后未重新选择却发送 prompt：%#v", calls)
		}
		harness.selectFirst(t)
		harness.send(t, "message-after-reselect", testUserID, "single", "重新选择后允许发送")
		harness.herdr.WaitCallCount(t, "agent.prompt", 1)
	})

	t.Run("企业微信重连不重放旧消息或通知", func(t *testing.T) {
		harness := newBridgeHarness(t, herdr.AgentStatusIdle)
		defer harness.stop(t)
		harness.selectFirst(t)
		harness.send(t, "message-before-wecom-disconnect", testUserID, "single", "重连前消息")
		harness.herdr.WaitCallCount(t, "agent.prompt", 1)
		agent := "codex"
		if delivered := harness.herdr.EmitStatus(herdr.AgentStatusEvent{
			PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusBlocked, Agent: &agent,
		}); delivered != 1 {
			t.Fatalf("blocked 事件写入订阅数 = %d, want 1", delivered)
		}
		harness.wecom.WaitRequestCount(t, "aibot_send_msg", 2)
		harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 2)
		sentBeforeDisconnect := len(harness.wecom.Requests("aibot_send_msg"))

		harness.wecom.SendDisconnectedEvent(t)
		harness.wecom.WaitSubscribeCount(t, 2)
		assertStableCount(t, func() int { return len(harness.herdr.Calls("agent.prompt")) }, 1)
		assertStableCount(t, func() int { return len(harness.wecom.Requests("aibot_send_msg")) }, sentBeforeDisconnect)

		harness.herdr.SetSnapshot(integrationSnapshot("session-1", herdr.AgentStatusIdle))
		harness.send(t, "message-after-wecom-reconnect", testUserID, "single", "重连后新消息")
		harness.herdr.WaitCallCount(t, "agent.prompt", 2)
	})
}

func TestApplicationHarnessCleanupReleasesProcessLock(t *testing.T) {
	t.Run("依赖测试清理停止应用", func(t *testing.T) {
		herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("cleanup-1", herdr.AgentStatusWorking))
		weComServer := testkit.NewWeComServer(t, testBotID, testSecret)
		application := startApplication(t, herdrServer.SocketPath(), weComServer.Endpoint())
		weComServer.WaitSubscribeCount(t, 1)
		if err := application.ctx.Err(); err != nil {
			t.Fatalf("应用提前停止：%v", err)
		}
	})

	herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("cleanup-2", herdr.AgentStatusWorking))
	weComServer := testkit.NewWeComServer(t, testBotID, testSecret)
	application := startApplication(t, herdrServer.SocketPath(), weComServer.Endpoint())
	weComServer.WaitSubscribeCount(t, 1)
	application.stop(t)
	application.stop(t)
}

type bridgeHarness struct {
	ctx   context.Context
	run   *applicationRun
	herdr *testkit.HerdrServer
	wecom *testkit.WeComServer
}

func newBridgeHarness(t *testing.T, status herdr.AgentStatus) *bridgeHarness {
	t.Helper()
	herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("session-1", status))
	weComServer := testkit.NewWeComServer(t, testBotID, testSecret)
	application := startApplication(t, herdrServer.SocketPath(), weComServer.Endpoint())
	weComServer.WaitSubscribeCount(t, 1)
	herdrServer.WaitCallCount(t, "ping", 1)
	herdrServer.WaitCallCount(t, "session.snapshot", 2)
	herdrServer.WaitSubscription(t, herdr.LifecycleSubscriptions())
	herdrServer.WaitSubscription(t, []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}})
	harness := &bridgeHarness{ctx: application.ctx, run: application, herdr: herdrServer, wecom: weComServer}
	harness.waitReady(t)
	return harness
}

func (h *bridgeHarness) stop(t *testing.T) { h.run.stop(t) }

func (h *bridgeHarness) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	nonce := time.Now().UnixNano()
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		reply := h.send(t, fmt.Sprintf("message-readiness-%d-%d", nonce, attempt), testUserID, "single", "/ls")
		if strings.Contains(reply.Content, "pane-1") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.run.stop(t)
	t.Fatal("BridgeService 未在上限内接管启动快照")
}

func (h *bridgeHarness) send(t *testing.T, messageID, userID, chatType, content string) testkit.WeComRequest {
	t.Helper()
	before := len(h.wecom.Requests("aibot_respond_msg"))
	completedBefore := len(h.wecom.CompletedRequests("aibot_respond_msg"))
	requestID := h.wecom.InjectText(t, messageID, userID, chatType, content)
	requests := h.wecom.WaitRequestCount(t, "aibot_respond_msg", before+1)
	h.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", completedBefore+1)
	for index := len(requests) - 1; index >= 0; index-- {
		if requests[index].RequestID == requestID {
			return requests[index]
		}
	}
	t.Fatalf("未找到回调 %s 的企业微信回复", requestID)
	return testkit.WeComRequest{}
}

func (h *bridgeHarness) selectFirst(t *testing.T) {
	t.Helper()
	h.send(t, "message-list-"+fmt.Sprint(time.Now().UnixNano()), testUserID, "single", "/ls")
	h.send(t, "message-select-"+fmt.Sprint(time.Now().UnixNano()), testUserID, "single", "/sel 1")
}

func assertCallParams(t *testing.T, call testkit.HerdrCall, want map[string]any) {
	t.Helper()
	var params map[string]any
	if err := json.Unmarshal(call.Params, &params); err != nil {
		t.Fatalf("解析 %s 参数：%v", call.Method, err)
	}
	for key, value := range want {
		if fmt.Sprint(params[key]) != fmt.Sprint(value) {
			t.Fatalf("%s 参数 %s = %#v, want %#v；完整参数 %#v", call.Method, key, params[key], value, params)
		}
	}
}

func assertStableCount(t *testing.T, count func() int, want int) {
	t.Helper()
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := count(); got != want {
			t.Fatalf("调用数 = %d, want stable %d", got, want)
		}
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func numberedLines(count int) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index+1)
	}
	return lines
}

func assertRecentNotification(t *testing.T, messages []testkit.WeComRequest) {
	t.Helper()
	var content strings.Builder
	for _, message := range messages {
		if message.ChatID != testUserID || message.ChatType != 1 {
			t.Fatalf("主动消息目标 = chatid %q chat_type %d, want %q/1", message.ChatID, message.ChatType, testUserID)
		}
		content.WriteString(message.Content)
		content.WriteByte('\n')
	}
	joined := content.String()
	if !strings.Contains(joined, "line-081") || !strings.Contains(joined, "line-180") || strings.Contains(joined, "line-080") {
		t.Fatalf("主动通知不是最后 100 行：%q", joined)
	}
}

type applicationRun struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	err        error
	stopErr    error
	stopOnce   sync.Once
	reportOnce sync.Once
}

func startApplication(t *testing.T, socketPath, endpoint string) *applicationRun {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	contents, err := json.Marshal(map[string]any{
		"wecom": map[string]any{"bot_id": testBotID, "allowed_user_id": testUserID},
		"herdr": map[string]any{"session": "", "socket_path": socketPath},
		"log":   map[string]any{"level": "error"},
	})
	if err != nil {
		t.Fatalf("编码集成测试配置：%v", err)
	}
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatalf("写入集成测试配置：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	application := &applicationRun{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := app.Run(ctx, app.Options{
			ConfigPath:    configPath,
			Getenv:        func(string) string { return testSecret },
			Stdout:        io.Discard,
			Stderr:        io.Discard,
			WeComEndpoint: endpoint,
		})
		application.mu.Lock()
		application.err = err
		application.mu.Unlock()
		close(application.done)
	}()
	t.Cleanup(func() { application.stop(t) })
	return application
}

func (a *applicationRun) stop(t *testing.T) {
	t.Helper()
	a.stopOnce.Do(func() {
		a.cancel()
		select {
		case <-a.done:
			a.mu.Lock()
			a.stopErr = a.err
			a.mu.Unlock()
		case <-time.After(3 * time.Second):
			a.mu.Lock()
			a.stopErr = fmt.Errorf("app.Run() 未在集成测试退出上限内停止")
			a.mu.Unlock()
		}
	})
	a.reportOnce.Do(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.stopErr != nil {
			t.Errorf("应用测试清理失败：%v", a.stopErr)
		}
	})
}

func integrationSnapshot(sessionID string, status herdr.AgentStatus) herdr.Snapshot {
	agent := "codex"
	display := "Codex"
	title := "实现 Herdr Pal"
	session := &herdr.AgentSession{Source: "codex", Agent: agent, Kind: "id", Value: sessionID}
	return herdr.Snapshot{
		Version: "0.8.0-test", Protocol: herdr.RequiredProtocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "项目"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "开发"}},
		Panes: []herdr.Pane{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, DisplayAgent: &display, AgentStatus: status, AgentSession: session,
		}},
		Agents: []herdr.AgentInfo{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, DisplayAgent: &display, AgentStatus: status, AgentSession: session,
		}},
	}
}
