package integration_test

import (
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

func TestHPRPProductionBridgeEndToEnd(t *testing.T) {
	harness := newHPAPHarness(t)
	defer harness.stop(t)

	issued := harness.issueKey(t, "user-bridge", "home", []string{"127.0.0.1"})
	harness.tokens = append(harness.tokens, issued.Token)
	fakeHerdr := testkit.NewHerdrServer(t, integrationSnapshot("session-production", herdr.AgentStatusIdle))
	fakeHerdr.SetOutput([]string{"production-terminal-output"})
	harness.pals = append(harness.pals, startHPAPPal(t, harness.relayURL, issued.Token, fakeHerdr.SocketPath()))

	harness.waitConnections(t, 1)
	harness.waitSessions(t, 1)
	fakeHerdr.WaitSubscription(t, []herdr.SubscriptionSpec{
		{Type: "pane.created"},
		{Type: "pane.closed"},
		{Type: "pane.updated"},
		{Type: "pane.exited"},
		{Type: "pane.agent_detected"},
	})
	fakeHerdr.WaitSubscription(t, []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}})

	listReply := harness.sendText(t, "production-list", "user-bridge", "/ls")
	for _, expected := range []string{"[home/1]", "项目/开发", "Codex"} {
		if !strings.Contains(listReply, expected) {
			t.Fatalf("生产链路 /ls 回复 %q 缺少 %q", listReply, expected)
		}
	}

	selectReply := harness.sendText(t, "production-select", "user-bridge", "/1")
	if !strings.Contains(selectReply, "production-terminal-output") || !strings.Contains(selectReply, "[终端输出#1]") {
		t.Fatalf("生产链路 /1 回复 = %q", selectReply)
	}

	promptReply := harness.sendText(t, "production-prompt", "user-bridge", "通过 HPRP 执行任务")
	if !strings.Contains(promptReply, "working") {
		t.Fatalf("生产链路 prompt 回复 = %q", promptReply)
	}
	promptCalls := fakeHerdr.WaitCallCount(t, "agent.prompt", 1)
	assertCallParams(t, promptCalls[0], map[string]any{"target": "pane-1", "text": "通过 HPRP 执行任务"})

	agent, displayAgent, title := "codex", "Codex", "实现 Herdr Pal"
	if delivered := fakeHerdr.EmitStatus(herdr.AgentStatusEvent{
		PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusDone,
		Agent: &agent, DisplayAgent: &displayAgent, Title: &title,
	}); delivered != 1 {
		t.Fatalf("生产链路 done 事件写入订阅数 = %d, want 1", delivered)
	}
	pushes := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 1)
	if content := pushes[len(pushes)-1].Content; !strings.Contains(content, "已完成") {
		t.Fatalf("生产链路 done 通知 = %q", content)
	}
}

func (harness *hpapHarness) sendText(t *testing.T, messageID, userID, content string) string {
	t.Helper()
	completedBefore := len(harness.wecom.CompletedRequests("aibot_respond_msg"))
	requestID := harness.wecom.InjectText(t, messageID, userID, "single", content)
	responses := harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", completedBefore+1)
	for _, response := range responses {
		if response.RequestID == requestID {
			return response.Content
		}
	}
	t.Fatalf("企业微信请求 %s 没有对应回复，共收到 %d 条", requestID, len(responses))
	return ""
}
