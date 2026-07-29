package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/terminalimage"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestServiceRejectsUnauthorizedAndGroupBeforeParsing(t *testing.T) {
	service, fake := newTestService(t)
	for _, message := range []wecom.IncomingText{
		{RequestID: "request-user", MessageID: "message-user", UserID: "other", ChatType: "single", Content: "/unknown secret"},
		{RequestID: "request-group", MessageID: "message-group", UserID: "user-1", ChatType: "group", Content: "/unknown secret"},
	} {
		service.HandleMessage(context.Background(), message)
	}
	if fake.callCount() != 0 {
		t.Fatalf("unauthorized input called Herdr: %#v", fake)
	}
	if got := fakeIMFromService(t, service).replyCount(); got != 0 {
		t.Fatalf("unauthorized input replied %d times", got)
	}
}

func TestServiceSelectTargetUsesStableIdentity(t *testing.T) {
	service, _ := newTestService(t)
	targets := service.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("CurrentTargets() = %#v", targets)
	}
	if err := service.SelectTarget(targets[0].PaneID, targets[0].OccupantKey); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	if selected, err := service.SelectedTarget(); err != nil || selected != targets[0] {
		t.Fatalf("SelectedTarget() = %#v, %v", selected, err)
	}
	if err := service.SelectTarget(targets[0].PaneID, "stale"); !errors.Is(err, session.ErrListSnapshotExpired) {
		t.Fatalf("SelectTarget(stale) error = %v", err)
	}
}

func TestServiceCurrentTargetsEmptyWhenHerdrUnavailable(t *testing.T) {
	service, _ := newTestService(t)
	if len(service.CurrentTargets()) != 1 {
		t.Fatal("healthy service should expose current target")
	}
	service.SetHerdr(nil)
	if targets := service.CurrentTargets(); len(targets) != 0 {
		t.Fatalf("degraded CurrentTargets() = %#v", targets)
	}
}

func TestServiceImageConReturnsSamePageTextAndPNG(t *testing.T) {
	service, fake := newTestService(t)
	recorder := &terminalRecorder{}
	service.im = recorder
	renderer := &fakeTerminalRenderer{result: terminalimage.Result{PNG: []byte("png"), Width: 80, Height: 34}}
	if err := service.SetTerminalRenderer(renderer); err != nil {
		t.Fatal(err)
	}
	target := service.CurrentTargets()[0]
	if err := service.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatal(err)
	}
	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: "\x1b[31m红色\x1b[0m\n第二行"}, nil)
	message := incoming("image-con", "/con")
	message.OutputMode = im.OutputModeImage
	service.HandleMessage(context.Background(), message)

	content := recorder.singleReply(t)
	if content.Mode != im.OutputModeImage || content.Text != "红色\n第二行" || content.Image == nil ||
		string(content.Image.Data) != "png" || content.Page == nil || content.Page.Current != 1 || content.Page.Total != 1 {
		t.Fatalf("terminal reply = %#v", content)
	}
	if got := renderer.lastANSI(); got != "\x1b[31m红色\x1b[0m\n第二行" {
		t.Fatalf("renderer ANSI = %q", got)
	}
}

func TestServiceNotificationSnapshotDoesNotChangeInteractivePage(t *testing.T) {
	service, fake := newTestService(t)
	target := service.CurrentTargets()[0]
	if err := service.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatal(err)
	}
	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: textLines(100, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("snapshot-con", "/con"))
	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: textLines(0, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("snapshot-pageup", "/pageup"))

	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: "独立通知快照"}, nil)
	content, err := service.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil || content.Text != "独立通知快照" {
		t.Fatalf("ReadTerminalSnapshot() = %#v, %v", content, err)
	}
	service.HandleMessage(context.Background(), incoming("snapshot-pagedown", "/pagedn"))
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "line-100") {
		t.Fatalf("notification snapshot changed interactive page: %q", reply)
	}
}

func TestServiceImageRenderFailureReturnsSameReadText(t *testing.T) {
	service, fake := newTestService(t)
	renderErr := errors.New("render failed")
	if err := service.SetTerminalRenderer(&fakeTerminalRenderer{err: renderErr}); err != nil {
		t.Fatal(err)
	}
	target := service.CurrentTargets()[0]
	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: "\x1b[32m可审计文本\x1b[0m"}, nil)
	content, err := service.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeImage, panel.PageSize)
	if !errors.Is(err, renderErr) || content.Mode != im.OutputModeText || content.Text != "可审计文本" || content.Image != nil {
		t.Fatalf("ReadTerminalSnapshot() = %#v, %v", content, err)
	}
}

func TestServiceNotificationSnapshotRejectsTargetChangedDuringRender(t *testing.T) {
	service, fake := newTestService(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	renderer := &fakeTerminalRenderer{
		result:  terminalimage.Result{PNG: []byte("png"), Width: 8, Height: 17},
		started: started, block: release,
	}
	if err := service.SetTerminalRenderer(renderer); err != nil {
		t.Fatal(err)
	}
	target := service.CurrentTargets()[0]
	fake.setRead(herdr.ReadResult{PaneID: target.PaneID, Text: "snapshot"}, nil)
	type snapshotResult struct {
		content im.TerminalContent
		err     error
	}
	result := make(chan snapshotResult, 1)
	go func() {
		content, err := service.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeImage, panel.PageSize)
		result <- snapshotResult{content: content, err: err}
	}()
	awaitSignal(t, started, "terminal renderer")
	service.ReplaceSnapshot(replacedSnapshot(), false)
	close(release)
	got := <-result
	if !errors.Is(got.err, session.ErrListSnapshotExpired) || got.content.Text != "" {
		t.Fatalf("ReadTerminalSnapshot() = %#v, %v", got.content, got.err)
	}
}

func TestServiceLogsRejectedAndDuplicateMessagesWithoutSensitiveValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service, fake := newTestServiceWithLogger(t, logger)

	service.HandleMessage(context.Background(), wecom.IncomingText{
		RequestID: "request-rejected", MessageID: "message-rejected", UserID: "unknown-user-sensitive",
		ChatType: "single", Content: "prompt-sensitive",
	})
	service.HandleMessage(context.Background(), wecom.IncomingText{
		RequestID: "request-group", MessageID: "message-group", UserID: "user-1",
		ChatType: "group", Content: "group-sensitive",
	})
	message := incoming("duplicate-sensitive-id", "/ls")
	service.HandleMessage(context.Background(), message)
	service.HandleMessage(context.Background(), message)

	if fake.callCount() != 0 {
		t.Fatalf("日志场景不应调用 Herdr：%#v", fake)
	}
	output := logs.String()
	for _, want := range []string{"企业微信消息被策略拒绝", "reason=user_mismatch", "reason=chat_type", "企业微信重复消息已忽略"} {
		if !strings.Contains(output, want) {
			t.Fatalf("日志缺少 %q：%q", want, output)
		}
	}
	for _, forbidden := range []string{"unknown-user-sensitive", "prompt-sensitive", "group-sensitive", "duplicate-sensitive-id"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("日志泄露敏感值 %q：%q", forbidden, output)
		}
	}
}

func TestServiceDeduplicatesAndRejectsEmptyMessageID(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	message := incoming("same-id", "保留原文  \n")
	service.HandleMessage(context.Background(), message)
	service.HandleMessage(context.Background(), message)
	if got := len(fake.prompts()); got != 1 {
		t.Fatalf("prompt calls = %d, want 1", got)
	}
	service.HandleMessage(context.Background(), incoming("", "ignored"))
	if got := len(fake.prompts()); got != 1 {
		t.Fatalf("empty message id sent prompt: %#v", fake.prompts())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "消息标识") {
		t.Fatalf("empty message ID reply = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceUnknownCommandShowsUsageWithoutHerdr(t *testing.T) {
	service, fake := newTestService(t)
	service.HandleMessage(context.Background(), incoming("unknown", "/unknown secret-prompt"))
	if fake.callCount() != 0 {
		t.Fatalf("unknown command called Herdr: %#v", fake)
	}
	got := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(got, "用法") || strings.Contains(got, "secret-prompt") {
		t.Fatalf("usage reply = %q", got)
	}
}

func TestServiceHelpDoesNotCallHerdr(t *testing.T) {
	service, fake := newTestService(t)

	service.HandleMessage(context.Background(), incoming("help", "/help"))

	if fake.callCount() != 0 {
		t.Fatalf("/help called Herdr: %#v", fake.callOrder())
	}
	reply := fakeIMFromService(t, service).lastReply()
	for _, want := range []string{"/help", "/N", "/key", "/slash"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("/help reply = %q, want %q", reply, want)
		}
	}
}

func TestServiceSelectShorthandUsesListSnapshot(t *testing.T) {
	service, fake := newTestService(t)
	service.HandleMessage(context.Background(), incoming("short-list", "/ls"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "选择后的终端内容"}

	service.HandleMessage(context.Background(), incoming("short-select", "/1"))

	selected, err := service.registry.ValidateSelected()
	if err != nil || selected.PaneID != "pane-1" {
		t.Fatalf("/<NUM> selection = %#v, %v, want pane-1", selected, err)
	}
	if reads := fake.reads(); len(reads) != 1 || reads[0].target != "pane-1" || reads[0].lines != panel.PageSize {
		t.Fatalf("/<NUM> reads = %#v, want immediate /con", reads)
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "选择后的终端内容") || !strings.Contains(reply, "页码:[1/1]") {
		t.Fatalf("/<NUM> reply = %q, want immediate terminal page", reply)
	}
}

func TestServiceSlashPromptUsesNormalPromptFlow(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)

	service.HandleMessage(context.Background(), incoming("slash-prompt", "/slash clear"))

	if got := fake.prompts(); len(got) != 1 || got[0].target != "pane-1" || got[0].text != "/clear" {
		t.Fatalf("/slash prompt calls = %#v, want pane-1 /clear", got)
	}
	if got := strings.Join(fake.callOrder(), ","); got != "get,prompt" {
		t.Fatalf("/slash call order = %q, want get,prompt", got)
	}
}

func TestServiceDegradedAllowsListButPausesHerdrActions(t *testing.T) {
	service, fake := newTestService(t)
	service.SetHerdr(nil)
	for index, content := range []string{"/ls", "/con", "/key enter", "prompt"} {
		service.HandleMessage(context.Background(), incoming(fmt.Sprintf("degraded-%d", index), content))
	}
	if fake.callCount() != 0 {
		t.Fatalf("degraded service called Herdr: %#v", fake)
	}
	im := fakeIMFromService(t, service)
	if !strings.Contains(im.replies[0], "Agent") || !strings.Contains(im.lastReply(), "暂不可用") {
		t.Fatalf("degraded replies = %#v", im.replies)
	}
}

func TestServiceListAndSelectionResetPanel(t *testing.T) {
	service, _ := newTestService(t)
	service.HandleMessage(context.Background(), incoming("list", "/ls"))
	list := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(list, "1.") || !strings.Contains(list, "workspace-1") || !strings.Contains(list, "状态：working ⏳") || strings.Contains(list, "当前选择") {
		t.Fatalf("list reply = %q", list)
	}
	service.HandleMessage(context.Background(), incoming("select", "/sel 1"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "[终端输出]") {
		t.Fatalf("select reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("list-selected", "/ls"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "当前选择") {
		t.Fatalf("selected list = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceListRefreshesAgentSessionBeforeSelection(t *testing.T) {
	service, fake := newTestService(t)
	service.registry.Replace(testSnapshotWithSession("session-old"), false)
	currentSnapshot := testSnapshotWithSession("session-new")
	currentSnapshot.Panes[0].AgentStatus = herdr.AgentStatusIdle
	fake.setSnapshot(currentSnapshot)
	current := agentInfo("pane-1", "terminal-1", "codex")
	current.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-new"}
	fake.setAgent(current)
	fake.promptResult = changedAgent(current, herdr.AgentStatusWorking)

	service.HandleMessage(context.Background(), incoming("session-refresh-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("session-refresh-select", "/sel 1"))
	service.HandleMessage(context.Background(), incoming("session-refresh-prompt", "继续处理"))

	if got := fake.snapshotCount(); got != 1 {
		t.Fatalf("/ls snapshot calls = %d, want 1", got)
	}
	if got := fake.prompts(); len(got) != 1 || got[0].target != "pane-1" {
		t.Fatalf("新会话选择后 prompt calls = %#v", got)
	}
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "目标 Agent 已变化") {
		t.Fatalf("新会话重新选择后仍被判定失效：%q", reply)
	}
}

func TestServiceListIncludesStableHierarchyTitleAndCurrentSelection(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := herdr.Snapshot{
		Protocol:   herdr.RequiredProtocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-2", Number: 2, Label: "工作区二"}, {WorkspaceID: "workspace-1", Number: 1, Label: "工作区一"}},
		Tabs:       []herdr.Tab{{TabID: "tab-2", WorkspaceID: "workspace-2", Number: 2, Label: "标签二"}, {TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "标签一"}},
		Panes: []herdr.Pane{
			{PaneID: "pane-z", TerminalID: "terminal-z", WorkspaceID: "workspace-2", TabID: "tab-2", Agent: stringRef("codex"), DisplayAgent: stringRef("Codex"), Title: stringRef("第二项"), AgentStatus: herdr.AgentStatusDone},
			{PaneID: "pane-a", TerminalID: "terminal-a", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), Title: stringRef("第一项\n```注入"), AgentStatus: herdr.AgentStatusBlocked},
		},
	}
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	service.HandleMessage(context.Background(), incoming("list-details", "/ls"))
	list := fakeIMFromService(t, service).lastReply()
	for _, want := range []string{"1. Claude", "标题：第一项 ``\u200b`注入", "工作区一/标签一", "状态：blocked ⁉️", "面板：pane-a", "2. Codex", "标题：第二项", "工作区二/标签二", "状态：done ✅", "面板：pane-z"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list missing %q: %q", want, list)
		}
	}
	fake.setRead(herdr.ReadResult{PaneID: "pane-a", Text: "第一项终端"}, nil)
	service.HandleMessage(context.Background(), incoming("list-select", "/sel 1"))
	service.HandleMessage(context.Background(), incoming("list-current", "/ls"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "1. Claude（当前选择）") {
		t.Fatalf("current marker missing: %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServicePromptPreservesBytesAndChecksLiveAgent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	prompt := "  keep\nall bytes  "
	service.HandleMessage(context.Background(), incoming("prompt", prompt))
	if got := fake.prompts(); len(got) != 1 || got[0].text != prompt || got[0].target != "pane-1" {
		t.Fatalf("prompt calls = %#v", got)
	}
	if got := fake.gets(); len(got) != 1 || got[0] != "pane-1" {
		t.Fatalf("GetAgent targets = %#v, want pane-1", got)
	}
	if got := fake.callOrder(); strings.Join(got, ",") != "get,prompt" {
		t.Fatalf("call order = %#v, want get,prompt", got)
	}
	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "已发送") || !strings.Contains(reply, "working") || strings.Contains(reply, "keep") {
		t.Fatalf("prompt reply = %q", reply)
	}
}

func TestServicePromptRebindsSessionChangedAfterSubmission(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := testSnapshotWithSession("session-old")
	snapshot.Panes[0].AgentStatus = herdr.AgentStatusIdle
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	current := agentInfo("pane-1", "terminal-1", "codex")
	current.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-old"}
	fake.setAgent(current)
	changed := changedAgent(current, herdr.AgentStatusWorking)
	changed.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-new"}
	fake.promptResult = changed
	selectTarget(t, service)

	service.HandleMessage(context.Background(), incoming("prompt-session-rollover", "继续处理"))

	if got := fake.prompts(); len(got) != 1 || got[0].target != "pane-1" {
		t.Fatalf("prompt calls = %#v", got)
	}
	reply := fakeIMFromService(t, service).lastReply()
	for _, want := range []string{"已发送", "会话", "已自动选择"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("发送后会话切换回复缺少 %q：%q", want, reply)
		}
	}
	if strings.Contains(reply, "/ls") || strings.Contains(reply, "/sel") {
		t.Fatalf("自动选择新会话后仍要求手工选择：%q", reply)
	}
	selected, err := service.registry.ValidateSelected()
	if err != nil || !session.MatchesAgent(selected, changed) {
		t.Fatalf("发送后未选中新会话：selected=%#v err=%v", selected, err)
	}

	currentNewSession := changed
	currentNewSession.AgentStatus = herdr.AgentStatusIdle
	fake.setAgent(currentNewSession)
	fake.promptResult = changedAgent(currentNewSession, herdr.AgentStatusWorking)
	service.HandleMessage(context.Background(), incoming("prompt-session-rollover-next", "继续下一步"))
	if got := fake.prompts(); len(got) != 2 || got[1].target != "pane-1" {
		t.Fatalf("新会话后续 prompt calls = %#v", got)
	}
}

func TestServicePromptOnlyAcceptsIdleAndDone(t *testing.T) {
	tests := []struct {
		status herdr.AgentStatus
		sent   bool
	}{
		{status: herdr.AgentStatusIdle, sent: true},
		{status: herdr.AgentStatusDone, sent: true},
		{status: herdr.AgentStatusWorking, sent: false},
		{status: herdr.AgentStatusBlocked, sent: false},
		{status: herdr.AgentStatusUnknown, sent: false},
	}

	for _, test := range tests {
		t.Run(string(test.status), func(t *testing.T) {
			service, fake := newTestService(t)
			selectTarget(t, service)
			current := agentInfo("pane-1", "terminal-1", "codex")
			current.AgentStatus = test.status
			fake.setAgent(current)
			fake.promptResult = changedAgent(current, herdr.AgentStatusWorking)

			service.HandleMessage(context.Background(), incoming("prompt-status-"+string(test.status), "prompt"))

			if got := len(fake.prompts()); (got == 1) != test.sent {
				t.Fatalf("prompt calls = %d, sent = %v", got, test.sent)
			}
			reply := fakeIMFromService(t, service).lastReply()
			if test.sent {
				if !strings.Contains(reply, "working") {
					t.Fatalf("success reply = %q", reply)
				}
			} else if !strings.Contains(reply, string(test.status)) {
				t.Fatalf("rejected reply = %q, want status %s", reply, test.status)
			}
		})
	}
}

func TestServicePromptStallSendsOneEnterAndWaitsForChange(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	current := fake.currentAgent()
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}
	fake.waitResult = changedAgent(current, herdr.AgentStatusWorking)

	service.HandleMessage(context.Background(), incoming("prompt-stall-recover", "prompt"))

	if got := strings.Join(fake.callOrder(), ","); got != "get,prompt,get,key,wait" {
		t.Fatalf("call order = %q", got)
	}
	if keys := fake.keys(); len(keys) != 1 || keys[0].key != "enter" {
		t.Fatalf("keys = %#v", keys)
	}
	waits := fake.waits()
	if len(waits) != 1 || waits[0].baseline != current.StateChangeSeq || waits[0].timeout != 5*time.Second {
		t.Fatalf("waits = %#v", waits)
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "working") {
		t.Fatalf("reply = %q", reply)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Key() != "enter" || audits[0].Result() != policy.AuditResultSent {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestServicePromptStallSkipsEnterWhenRecheckAlreadyChanged(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	current := fake.currentAgent()
	changed := changedAgent(current, herdr.AgentStatusWorking)
	fake.setGetResults(agentGetResult{agent: current}, agentGetResult{agent: changed})
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}

	service.HandleMessage(context.Background(), incoming("prompt-stall-late-change", "prompt"))

	if len(fake.keys()) != 0 || len(fake.waits()) != 0 {
		t.Fatalf("unexpected recovery calls: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "working") {
		t.Fatalf("reply = %q", reply)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultRejected {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestServicePromptStallTimesOutAfterOneEnter(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}
	fake.waitErr = herdr.ErrAgentStateChangeTimeout

	service.HandleMessage(context.Background(), incoming("prompt-stall-timeout", "prompt"))

	if len(fake.keys()) != 1 || len(fake.waits()) != 1 {
		t.Fatalf("recovery calls: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "未生效") || strings.Contains(reply, "已发送") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestServicePromptStallDoesNotRetryOtherErrors(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.promptErr = errors.New("private prompt failure")

	service.HandleMessage(context.Background(), incoming("prompt-other-error", "prompt"))

	if len(fake.keys()) != 0 || len(fake.waits()) != 0 || len(fakeAuditFromService(t, service).records()) != 0 {
		t.Fatalf("unexpected recovery: keys=%#v waits=%#v audits=%#v", fake.keys(), fake.waits(), fakeAuditFromService(t, service).records())
	}
}

func TestServicePromptStallInvalidatesReplacedOccupant(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	current := fake.currentAgent()
	replacement := agentInfo("pane-1", "terminal-replaced", "claude")
	fake.setGetResults(agentGetResult{agent: current}, agentGetResult{agent: replacement})
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}

	service.HandleMessage(context.Background(), incoming("prompt-stall-replaced", "prompt"))

	if len(fake.keys()) != 0 || len(fake.waits()) != 0 {
		t.Fatalf("replacement reached recovery: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("replacement did not invalidate selection")
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultRejected {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestServicePromptStallInvalidatesReplacementObservedAfterEnter(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}
	fake.waitResult = agentInfo("pane-1", "terminal-replaced", "claude")

	service.HandleMessage(context.Background(), incoming("prompt-stall-replaced-after-enter", "prompt"))

	if len(fake.keys()) != 1 || len(fake.waits()) != 1 {
		t.Fatalf("recovery calls: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("replacement did not invalidate selection")
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "/ls") || !strings.Contains(reply, "/sel") {
		t.Fatalf("replacement reply = %q", reply)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultSent {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestServicePromptStallAuditsFailedRecheck(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	current := fake.currentAgent()
	fake.setGetResults(agentGetResult{agent: current}, agentGetResult{err: errors.New("private get failure")})
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}

	service.HandleMessage(context.Background(), incoming("prompt-stall-recheck-failed", "prompt"))

	if len(fake.keys()) != 0 || len(fake.waits()) != 0 {
		t.Fatalf("unexpected recovery calls: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultFailed {
		t.Fatalf("audits = %#v", audits)
	}
	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "暂不可用") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestServicePromptStallAuditsFailedEnter(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled", Message: "no state change"}
	fake.keyErr = errors.New("private key failure")

	service.HandleMessage(context.Background(), incoming("prompt-stall-key-failed", "prompt"))

	if len(fake.keys()) != 1 || len(fake.waits()) != 0 {
		t.Fatalf("recovery calls: keys=%#v waits=%#v", fake.keys(), fake.waits())
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultFailed {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestServiceSingleKeyCommandsSendOnceWithoutConfirmation(t *testing.T) {
	for _, test := range []struct {
		content string
		key     string
	}{
		{"/key up", "up"}, {"/key down", "down"}, {"/enter", "enter"}, {"/key enter", "enter"},
		{"/key esc", "esc"}, {"/key space", "space"}, {"/key A", "A"},
	} {
		t.Run(test.content, func(t *testing.T) {
			service, fake := newTestService(t)
			selectTarget(t, service)
			fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: "single-key-console"}, nil)
			service.HandleMessage(context.Background(), incoming("key-"+test.key, test.content))
			if got := fake.keys(); len(got) != 1 || got[0].key != test.key {
				t.Fatalf("key calls = %#v", got)
			}
			if got := fake.keys(); got[0].target != "pane-1" {
				t.Fatalf("SendKey targets = %#v, want pane-1", got)
			}
			if got := fake.gets(); len(got) != 1 || got[0] != "pane-1" {
				t.Fatalf("GetAgent targets = %#v, want pane-1", got)
			}
			if got := strings.Join(fake.callOrder(), ","); got != "get,key,read" {
				t.Fatalf("key call order = %q, want get,key,read", got)
			}
			if got := fake.reads(); len(got) != 1 || got[0] != (readCall{target: "pane-1", lines: panel.PageSize}) {
				t.Fatalf("key read calls = %#v", got)
			}
			reply := fakeIMFromService(t, service).lastReply()
			if strings.Contains(reply, "确认") || !strings.Contains(reply, "single-key-console") {
				t.Fatalf("key reply = %q, want console without confirmation", reply)
			}
		})
	}
}

func TestServiceKeySequenceSendsInOrderWithIntervalsAndRefreshesContent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: "console-after-keys"}, nil)
	var intervals []time.Duration
	service.waitKeyInterval = func(_ context.Context, duration time.Duration) error {
		intervals = append(intervals, duration)
		return nil
	}

	service.HandleMessage(context.Background(), incoming("key-sequence", "/key down,sp dn space,"))

	keys := fake.keys()
	if len(keys) != 4 {
		t.Fatalf("key calls = %#v, want 4", keys)
	}
	for index, want := range []string{"down", "space", "down", "space"} {
		if keys[index] != (keyCall{target: "pane-1", key: want}) {
			t.Fatalf("key call %d = %#v, want %q", index, keys[index], want)
		}
	}
	if got := fake.gets(); len(got) != 4 {
		t.Fatalf("GetAgent calls = %#v, want 4", got)
	}
	if got := strings.Join(fake.callOrder(), ","); got != "get,key,get,key,get,key,get,key,read" {
		t.Fatalf("call order = %q", got)
	}
	if len(intervals) != 3 {
		t.Fatalf("intervals = %#v, want 3", intervals)
	}
	for _, interval := range intervals {
		if interval != 100*time.Millisecond {
			t.Fatalf("interval = %s, want 100ms", interval)
		}
	}
	if got := fake.reads(); len(got) != 1 || got[0] != (readCall{target: "pane-1", lines: panel.PageSize}) {
		t.Fatalf("read calls = %#v", got)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 4 {
		t.Fatalf("audits = %#v, want 4", audits)
	}
	for index, audit := range audits {
		if audit.Key() != []string{"down", "space", "down", "space"}[index] || audit.Result() != policy.AuditResultSent {
			t.Fatalf("audit %d = %#v", index, audit)
		}
	}
	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "4/4") || !strings.Contains(reply, "console-after-keys") {
		t.Fatalf("key sequence reply = %q", reply)
	}
	footerIndex := strings.Index(reply, "[终端输出]")
	summaryIndex := strings.Index(reply, "按键已发送")
	if !strings.HasPrefix(reply, "```\nconsole-after-keys") || footerIndex < 0 || summaryIndex < footerIndex {
		t.Fatalf("key sequence reply order = %q, want output, footer, then summary", reply)
	}
}

func TestServiceKeySequenceWaitsAfterLastKeyBeforeRefreshingContent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: "console-after-delay"}, nil)
	service.waitKeyInterval = func(context.Context, time.Duration) error { return nil }
	var delays []time.Duration
	service.waitKeyReadback = func(_ context.Context, duration time.Duration) error {
		delays = append(delays, duration)
		if got := len(fake.keys()); got != 2 {
			t.Fatalf("keys before readback wait = %d, want 2", got)
		}
		if got := len(fake.reads()); got != 0 {
			t.Fatalf("reads before readback wait = %d, want 0", got)
		}
		return nil
	}

	service.HandleMessage(context.Background(), incoming("key-readback-delay", "/key down space"))

	if len(delays) != 1 || delays[0] != 200*time.Millisecond {
		t.Fatalf("readback delays = %#v, want [200ms]", delays)
	}
	if got := fake.reads(); len(got) != 1 {
		t.Fatalf("read calls = %#v, want one call after delay", got)
	}
}

func TestServiceKeySequenceStopsOnSendFailureAndStillRefreshes(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.keyErrors = []error{nil, errors.New("private second key failure")}
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: "console-after-partial"}, nil)
	var intervals []time.Duration
	service.waitKeyInterval = func(_ context.Context, duration time.Duration) error {
		intervals = append(intervals, duration)
		return nil
	}

	service.HandleMessage(context.Background(), incoming("key-partial", "/key up down space"))

	if got := fake.keys(); len(got) != 2 || got[0].key != "up" || got[1].key != "down" {
		t.Fatalf("partial key calls = %#v", got)
	}
	if len(intervals) != 1 || intervals[0] != 100*time.Millisecond {
		t.Fatalf("partial intervals = %#v", intervals)
	}
	if got := strings.Join(fake.callOrder(), ","); got != "get,key,get,key,read" {
		t.Fatalf("partial call order = %q", got)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 2 || audits[0].Result() != policy.AuditResultSent || audits[1].Result() != policy.AuditResultFailed {
		t.Fatalf("partial audits = %#v", audits)
	}
	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "1/3") || !strings.Contains(reply, "console-after-partial") || !strings.Contains(reply, "后续未执行") {
		t.Fatalf("partial reply = %q", reply)
	}
}

func TestServiceKeySequenceStopsWhenOccupantChanges(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setGetResults(
		agentGetResult{agent: agentInfo("pane-1", "terminal-1", "codex")},
		agentGetResult{agent: agentInfo("pane-1", "terminal-1", "claude")},
	)
	service.waitKeyInterval = func(context.Context, time.Duration) error { return nil }

	service.HandleMessage(context.Background(), incoming("key-occupant-change", "/key up down space"))

	if got := fake.keys(); len(got) != 1 || got[0].key != "up" {
		t.Fatalf("occupant change key calls = %#v", got)
	}
	if got := fake.reads(); len(got) != 0 {
		t.Fatalf("occupant change read calls = %#v, want none", got)
	}
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 2 || audits[0].Result() != policy.AuditResultSent || audits[1].Result() != policy.AuditResultRejected {
		t.Fatalf("occupant change audits = %#v", audits)
	}
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("occupant change did not invalidate selection")
	}
	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "1/3") || !strings.Contains(reply, "目标 Agent 已变化") {
		t.Fatalf("occupant change reply = %q", reply)
	}
}

func TestServiceKeyReadFailureDoesNotMisreportSendFailure(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.readErr = errors.New("private read failure")

	service.HandleMessage(context.Background(), incoming("key-read-failure", "/key space"))

	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "按键已发送") || !strings.Contains(reply, "控制台读取失败") || strings.Contains(reply, "按键发送失败") {
		t.Fatalf("read failure reply = %q", reply)
	}
}

func TestServiceKeyAuditsEveryAttemptAfterSelection(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*fakeHerdr)
		wantResult policy.AuditResult
		wantKeys   int
	}{
		{name: "sent", wantResult: policy.AuditResultSent, wantKeys: 1},
		{name: "get failed", prepare: func(fake *fakeHerdr) { fake.getErr = errors.New("private get failure") }, wantResult: policy.AuditResultFailed},
		{name: "occupant rejected", prepare: func(fake *fakeHerdr) { fake.setAgent(agentInfo("pane-1", "terminal-1", "other")) }, wantResult: policy.AuditResultRejected},
		{name: "send failed", prepare: func(fake *fakeHerdr) { fake.keyErr = errors.New("private send failure") }, wantResult: policy.AuditResultFailed, wantKeys: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, fake := newTestService(t)
			selectTarget(t, service)
			target, err := service.registry.ValidateSelected()
			if err != nil {
				t.Fatal(err)
			}
			if test.prepare != nil {
				test.prepare(fake)
			}

			service.HandleMessage(context.Background(), incoming("key-audit-"+test.name, "/key enter"))

			audits := fakeAuditFromService(t, service).records()
			if len(audits) != 1 {
				t.Fatalf("audit records = %#v, want exactly one", audits)
			}
			audit := audits[0]
			if audit.UserID() != "user-1" || audit.PaneID() != target.PaneID ||
				audit.OccupantHash() != target.OccupantKey || audit.Key() != "enter" ||
				audit.At().IsZero() || audit.Result() != test.wantResult {
				t.Fatalf("audit = %#v, want safe selected-target fields and result %q", audit, test.wantResult)
			}
			if got := len(fake.keys()); got != test.wantKeys {
				t.Fatalf("SendKey calls = %d, want %d", got, test.wantKeys)
			}
		})
	}
}

func TestServiceKeyAuditIgnoresDuplicateUnauthorizedAndInvalidCommands(t *testing.T) {
	service, _ := newTestService(t)
	selectTarget(t, service)
	message := incoming("key-audit-once", "/enter")
	service.HandleMessage(context.Background(), message)
	service.HandleMessage(context.Background(), message)
	service.HandleMessage(context.Background(), wecom.IncomingText{
		RequestID: "request-key-unauthorized", MessageID: "key-unauthorized", UserID: "other", ChatType: "single", Content: "/enter",
	})
	service.HandleMessage(context.Background(), wecom.IncomingText{
		RequestID: "request-key-group", MessageID: "key-group", UserID: "user-1", ChatType: "group", Content: "/enter",
	})
	service.HandleMessage(context.Background(), incoming("key-invalid", "/key ctrl+c private-command"))

	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultSent {
		t.Fatalf("audit records = %#v, want only the first authorized unique key", audits)
	}
}

func TestServiceKeyAuditRecordsDegradedAttemptOnlyWithValidSelection(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	service.SetHerdr(nil)
	service.HandleMessage(context.Background(), incoming("key-degraded-selected", "/enter"))
	audits := fakeAuditFromService(t, service).records()
	if len(audits) != 1 || audits[0].Result() != policy.AuditResultFailed || audits[0].PaneID() != "pane-1" {
		t.Fatalf("selected degraded audits = %#v, want one failed pane-1 audit", audits)
	}
	if len(fake.keys()) != 0 {
		t.Fatalf("degraded key reached Herdr: %#v", fake.keys())
	}

	service, _ = newTestService(t)
	service.SetHerdr(nil)
	service.HandleMessage(context.Background(), incoming("key-degraded-unselected", "/enter"))
	if audits := fakeAuditFromService(t, service).records(); len(audits) != 0 {
		t.Fatalf("unselected degraded audits = %#v, want none", audits)
	}
}

func TestServiceKeyMismatchClearsSelectionWithoutSending(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setAgent(agentInfo("pane-1", "terminal-1", "other"))
	service.HandleMessage(context.Background(), incoming("key-mismatch", "/key enter"))
	if len(fake.keys()) != 0 || !strings.Contains(fakeIMFromService(t, service).lastReply(), "/sel") {
		t.Fatalf("mismatch keys=%#v reply=%q", fake.keys(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceInputMismatchDoesNotInvalidateNewSelection(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "prompt", content: "prompt"},
		{name: "key", content: "/key enter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fake := newTestService(t)
			snapshot := twoTargetSnapshot()
			service.registry.Replace(snapshot, false)
			fake.setSnapshot(snapshot)
			selectTarget(t, service)
			fake.setAgent(agentInfo("pane-1", "terminal-1", "other"))
			fake.blockGet = make(chan struct{})
			fake.getStarted = make(chan struct{}, 1)
			requestDone := make(chan struct{})
			go func() {
				service.HandleMessage(context.Background(), incoming("input-mismatch-"+test.name, test.content))
				close(requestDone)
			}()
			awaitSignal(t, fake.getStarted, "GetAgent")
			fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: "SECOND-PANEL"}, nil)
			selectDone := make(chan struct{})
			go func() {
				service.HandleMessage(context.Background(), incoming("input-mismatch-select-"+test.name, "/sel 2"))
				close(selectDone)
			}()
			waitForCondition(t, func() bool {
				service.opMu.Lock()
				defer service.opMu.Unlock()
				return service.inputBlocked > 0
			}, "/sel input barrier")
			close(fake.blockGet)
			awaitSignal(t, selectDone, "/sel B")
			awaitSignal(t, requestDone, "mismatched input")
			selected, err := service.registry.ValidateSelected()
			if err != nil || selected.PaneID != "pane-2" {
				t.Fatalf("new selection was invalidated: %#v, %v", selected, err)
			}
			if len(fake.prompts()) != 0 || len(fake.keys()) != 0 {
				t.Fatalf("mismatched input reached Herdr: prompts=%#v keys=%#v", fake.prompts(), fake.keys())
			}
		})
	}
}

func TestServiceReadMismatchDoesNotInvalidateNewSelectionOrPanel(t *testing.T) {
	for _, test := range []struct {
		name       string
		prepareOld func(*Service, *fakeHerdr)
		content    string
	}{
		{
			name:       "content",
			prepareOld: func(_ *Service, _ *fakeHerdr) {},
			content:    "/con",
		},
		{
			name: "pageup",
			prepareOld: func(service *Service, fake *fakeHerdr) {
				fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: namedLines("A", 100, 200)}, nil)
				service.HandleMessage(context.Background(), incoming("read-mismatch-pageup-content", "/con"))
			},
			content: "/pageup",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fake := newTestService(t)
			snapshot := twoTargetSnapshot()
			service.registry.Replace(snapshot, false)
			fake.setSnapshot(snapshot)
			selectTarget(t, service)
			test.prepareOld(service, fake)
			oldReadBlock := make(chan struct{})
			fake.setRead(herdr.ReadResult{PaneID: "wrong-pane", Text: "OLD-MISMATCH"}, oldReadBlock)
			fake.readStarted = make(chan struct{}, 1)
			oldDone := make(chan struct{})
			go func() {
				service.HandleMessage(context.Background(), incoming("read-mismatch-"+test.name, test.content))
				close(oldDone)
			}()
			awaitSignal(t, fake.readStarted, "old ReadRecent")
			fake.clearReadStarted()
			service.HandleMessage(context.Background(), incoming("read-mismatch-list-"+test.name, "/ls"))
			fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: namedLines("B", 100, 200)}, nil)
			service.HandleMessage(context.Background(), incoming("read-mismatch-select-"+test.name, "/sel 2"))
			service.HandleMessage(context.Background(), incoming("read-mismatch-new-"+test.name, "/con"))
			close(oldReadBlock)
			awaitSignal(t, oldDone, "old mismatched read")
			selected, err := service.registry.ValidateSelected()
			if err != nil || selected.PaneID != "pane-2" {
				t.Fatalf("new selection was invalidated: %#v, %v", selected, err)
			}
			service.stateMu.Lock()
			cached := strings.Join(service.panel.Render(), "\n")
			ready := service.panelReady
			service.stateMu.Unlock()
			if !ready || !strings.Contains(cached, "B-100") || strings.Contains(cached, "OLD-MISMATCH") {
				t.Fatalf("new panel was invalidated or replaced: ready=%t cache=%q", ready, cached)
			}
			service.HandleMessage(context.Background(), incoming("read-mismatch-pagedown-"+test.name, "/pagedn"))
			if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-MISMATCH") {
				t.Fatalf("old read leaked through pagedown: %q", reply)
			}
		})
	}
}

func TestServiceReplaceSnapshotWaitsForInputAndResetsInvalidSelection(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: namedLines("replace", 100, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("replace-preload-content", "/con"))
	assertPanelState(t, service, true, 0, "replace-100")
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("replace-blocked-prompt", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")
	replaceDone := make(chan session.ChangeSet, 1)
	go func() { replaceDone <- service.ReplaceSnapshot(replacedSnapshot(), false) }()
	waitForCondition(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.inputBlocked > 0
	}, "ReplaceSnapshot input barrier")
	before := fake.callCount()
	service.HandleMessage(context.Background(), incoming("replace-new-key", "/key enter"))
	if fake.callCount() != before {
		t.Fatalf("new input passed through ReplaceSnapshot: %#v", fake)
	}
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "Prompt")
	changes := awaitChangeSet(t, replaceDone, "ReplaceSnapshot")
	if !changes.SelectionInvalidated || len(changes.ReplacedTargets) != 1 {
		t.Fatalf("replacement changes = %#v", changes)
	}
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("replacement did not clear selection")
	}
	assertPanelState(t, service, false, 0, "")

	service.registry.Replace(testSnapshot(), false)
	service.HandleMessage(context.Background(), incoming("replace-reconnect-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("replace-reconnect-select", "/sel 1"))
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: namedLines("reconnect", 100, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("replace-reconnect-content", "/con"))
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: namedLines("reconnect", 0, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("replace-reconnect-pageup", "/pageup"))
	assertPanelState(t, service, true, 1, "reconnect-000")
	changes = service.ReplaceSnapshot(testSnapshot(), true)
	if !changes.SelectionInvalidated {
		t.Fatalf("reconnect changes = %#v", changes)
	}
	assertPanelState(t, service, false, 0, "")
	service.HandleMessage(context.Background(), incoming("replace-reconnect-pagedown", "/pagedn"))
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "reconnect-") {
		t.Fatalf("reconnect leaked cached panel through pagedown: %q", reply)
	}
}

func TestServiceReplaceSnapshotWaitsForKey(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockKey = make(chan struct{})
	fake.keyStarted = make(chan struct{}, 1)
	keyDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("replace-blocked-key", "/key enter"))
		close(keyDone)
	}()
	awaitSignal(t, fake.keyStarted, "SendKey")
	replaceDone := make(chan session.ChangeSet, 1)
	go func() { replaceDone <- service.ReplaceSnapshot(testSnapshot(), true) }()
	waitForCondition(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.inputBlocked > 0
	}, "ReplaceSnapshot key barrier")
	close(fake.blockKey)
	awaitSignal(t, keyDone, "SendKey")
	changes := awaitChangeSet(t, replaceDone, "ReplaceSnapshot")
	if !changes.SelectionInvalidated {
		t.Fatalf("reconnect changes = %#v", changes)
	}
}

func TestServiceReplaceSnapshotPreservingStatusUsesBarrierAndInvalidatesReplacement(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: namedLines("preserve", 100, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("preserve-preload-content", "/con"))
	assertPanelState(t, service, true, 0, "preserve-100")

	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("preserve-blocked-prompt", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")

	sameOccupant := testSnapshot()
	sameOccupant.Panes[0].AgentStatus = herdr.AgentStatusDone
	sameOccupant.Panes[0].Title = stringRef("更新后的标题")
	replaceDone := make(chan session.ChangeSet, 1)
	go func() { replaceDone <- service.ReplaceSnapshotPreservingStatus(sameOccupant) }()
	waitForCondition(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.inputBlocked > 0
	}, "ReplaceSnapshotPreservingStatus input barrier")
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "Prompt")
	changes := awaitChangeSet(t, replaceDone, "ReplaceSnapshotPreservingStatus")
	if changes.SelectionInvalidated || len(changes.ReplacedTargets) != 0 {
		t.Fatalf("same occupant changes = %#v", changes)
	}
	selected, err := service.registry.ValidateSelected()
	if err != nil || selected.Status != herdr.AgentStatusWorking || selected.Title != "更新后的标题" {
		t.Fatalf("same occupant selected = %#v, err = %v", selected, err)
	}
	assertPanelState(t, service, true, 0, "preserve-100")

	replacement := replacedSnapshot()
	replacement.Panes[0].AgentStatus = herdr.AgentStatusBlocked
	changes = service.ReplaceSnapshotPreservingStatus(replacement)
	if !changes.SelectionInvalidated || len(changes.ReplacedTargets) != 1 {
		t.Fatalf("replacement changes = %#v", changes)
	}
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("preserving replace did not clear replacement selection")
	}
	targets := service.registry.CreateListSnapshot()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusBlocked {
		t.Fatalf("replacement targets = %#v", targets)
	}
	assertPanelState(t, service, false, 0, "")
}

func TestServiceLiveMismatchClearsSelectionWithoutInput(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.agent = agentInfo("pane-1", "terminal-1", "other")
	service.HandleMessage(context.Background(), incoming("mismatch", "prompt"))
	if len(fake.prompts()) != 0 {
		t.Fatalf("mismatch sent prompt: %#v", fake.prompts())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/ls") {
		t.Fatalf("mismatch reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("mismatch-next", "again"))
	if len(fake.gets()) != 1 {
		t.Fatalf("cleared selection still queried agent: %#v", fake.gets())
	}
}

func TestServiceContentAndPaging(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	if got := fake.reads(); len(got) != 1 || got[0].lines != 100 {
		t.Fatalf("content reads = %#v", got)
	}
	if got := fake.reads(); got[0].target != "pane-1" {
		t.Fatalf("/con target = %#v, want pane-1", got)
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "页码:[1/1]") {
		t.Fatalf("content reply = %q", fakeIMFromService(t, service).lastReply())
	}
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("pageup", "/pageup"))
	if got := fake.reads(); len(got) != 2 || got[1].lines != 200 {
		t.Fatalf("pageup reads = %#v", got)
	}
	if got := fake.reads(); got[1].target != "pane-1" {
		t.Fatalf("/pageup target = %#v, want pane-1", got)
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "页码:[2/2]") {
		t.Fatalf("pageup reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("pagedown", "/pagedn"))
	if got := len(fake.reads()); got != 2 {
		t.Fatalf("pagedown unexpectedly read Herdr: %#v", fake.reads())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "页码:[1/2]") {
		t.Fatalf("pagedown reply = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceReselectSameTargetPreservesPanelForPageUp(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("reselect-content", "/con"))
	target, err := service.SelectedTarget()
	if err != nil {
		t.Fatal(err)
	}

	if err := service.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatal(err)
	}
	fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}, nil)
	service.HandleMessage(context.Background(), incoming("reselect-pageup", "/pageup"))

	if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "页码:[2/2]") || strings.Contains(reply, "终端内容已变化") {
		t.Fatalf("pageup after same target reselect = %q", reply)
	}
}

func TestServiceSelectResetsExistingPanelCache(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := twoTargetSnapshot()
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("select-content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("select-pageup", "/pageup"))
	service.HandleMessage(context.Background(), incoming("select-list-again", "/ls"))
	fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: "SECOND-PANEL"}, nil)
	service.HandleMessage(context.Background(), incoming("select-two", "/sel 2"))
	beforeReads := len(fake.reads())
	service.HandleMessage(context.Background(), incoming("select-pagedown", "/pagedn"))
	if len(fake.reads()) != beforeReads || !strings.Contains(fakeIMFromService(t, service).lastReply(), "最新") || strings.Contains(fakeIMFromService(t, service).lastReply(), "line-100") {
		t.Fatalf("selection retained old panel: reads=%#v reply=%q", fake.reads(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceBlockedReadCannotRestoreOldPanelAfterSelectionChanges(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := twoTargetSnapshot()
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	service.HandleMessage(context.Background(), incoming("read-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("read-select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "OLD-TERMINAL-CONTENT"}
	oldReadBlock := make(chan struct{})
	fake.blockRead = oldReadBlock
	fake.readStarted = make(chan struct{}, 1)
	readDone := make(chan struct{})
	go func() { service.HandleMessage(context.Background(), incoming("read-old", "/con")); close(readDone) }()
	awaitSignal(t, fake.readStarted, "ReadRecent")
	fake.clearReadStarted()
	service.HandleMessage(context.Background(), incoming("read-list-again", "/ls"))
	fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: "NEW-TERMINAL-CONTENT"}, nil)
	service.HandleMessage(context.Background(), incoming("read-select-two", "/sel 2"))
	close(oldReadBlock)
	awaitSignal(t, readDone, "old /con")
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-TERMINAL-CONTENT") || !strings.Contains(reply, "/con") {
		t.Fatalf("old read was applied after selection switch: %q", reply)
	}
	service.HandleMessage(context.Background(), incoming("read-after-pagedown", "/pagedn"))
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-TERMINAL-CONTENT") || !strings.Contains(reply, "最新") {
		t.Fatalf("old panel leaked after selection switch: %q", reply)
	}
}

func TestServiceBlockedReadCannotRestoreOldPanelAfterInvalidation(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "OLD-INVALIDATED-CONTENT"}
	fake.blockRead = make(chan struct{})
	fake.readStarted = make(chan struct{}, 1)
	readDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("invalidate-read", "/con"))
		close(readDone)
	}()
	awaitSignal(t, fake.readStarted, "ReadRecent")
	hookEntered, releaseHook := make(chan struct{}, 1), make(chan struct{})
	service.beforeInvalidateStateChange = func() {
		signal(hookEntered)
		<-releaseHook
	}
	invalidated := make(chan struct{})
	go func() { service.InvalidateSelection(); close(invalidated) }()
	awaitSignal(t, hookEntered, "InvalidateSelection hook")
	close(releaseHook)
	awaitSignal(t, invalidated, "InvalidateSelection")
	close(fake.blockRead)
	awaitSignal(t, readDone, "old /con")
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-INVALIDATED-CONTENT") || strings.Contains(reply, "[终端输出]") {
		t.Fatalf("old read was applied after invalidation: %q", reply)
	}
	service.HandleMessage(context.Background(), incoming("invalidate-read-pagedown", "/pagedn"))
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-INVALIDATED-CONTENT") || strings.Contains(reply, "[终端输出]") {
		t.Fatalf("old panel leaked after invalidation: %q", reply)
	}
}

func TestServiceContentNormalizesTerminalControls(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "\x1b]0;title\a\x1b[31mred\x1b[0m\rfinal\x00  \n"}
	service.HandleMessage(context.Background(), incoming("normalize", "/con"))
	reply := fakeIMFromService(t, service).lastReply()
	if strings.Contains(reply, "\x1b") || strings.Contains(reply, "red") || !strings.Contains(reply, "final") {
		t.Fatalf("terminal normalization reply = %q", reply)
	}
}

func TestServicePageUpExpandsReadsUntilMaximum(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(900, 1000)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	for page := 1; page < 10; page++ {
		fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(1000-(page+1)*100, 1000)}
		service.HandleMessage(context.Background(), incoming(fmt.Sprintf("pageup-%d", page), "/pageup"))
	}
	reads := fake.reads()
	if len(reads) != 10 {
		t.Fatalf("read count = %d, want 10", len(reads))
	}
	for index, call := range reads {
		want := (index + 1) * 100
		if call.lines != want {
			t.Fatalf("read %d lines = %d, want %d", index, call.lines, want)
		}
	}
	service.HandleMessage(context.Background(), incoming("pageup-oldest", "/pageup"))
	if len(fake.reads()) != 10 || !strings.Contains(fakeIMFromService(t, service).lastReply(), "最早") {
		t.Fatalf("oldest page behaviour reads=%#v reply=%q", fake.reads(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceReadPaneMismatchInvalidatesSelection(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "other-pane", Text: "content"}
	service.HandleMessage(context.Background(), incoming("wrong-pane", "/con"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/ls") {
		t.Fatalf("read mismatch reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("wrong-pane-next", "prompt"))
	if len(fake.gets()) != 0 {
		t.Fatalf("read mismatch retained selection: %#v", fake.gets())
	}
}

func TestNewServiceRejectsMissingDependencies(t *testing.T) {
	registry := &session.Registry{}
	guard, err := policy.NewGuard("user-1")
	if err != nil {
		t.Fatal(err)
	}
	deduper, err := policy.NewDeduper(time.Minute, 1, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	audit := &fakeKeyAuditSink{}
	if _, err := NewService(nil, &panel.Buffer{}, guard, deduper, &fakeIM{}, audit, discardTestLogger()); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil registry error = %v", err)
	}
	if _, err := NewService(registry, nil, guard, deduper, &fakeIM{}, audit, discardTestLogger()); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil panel error = %v", err)
	}
	if _, err := NewService(registry, &panel.Buffer{}, guard, deduper, &fakeIM{}, nil, discardTestLogger()); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil audit sink error = %v", err)
	}
	if _, err := NewService(registry, &panel.Buffer{}, guard, deduper, &fakeIM{}, audit, nil); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil logger error = %v", err)
	}
}

func TestServicePanelChangedResetsSelectionAndCache(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "different"}
	service.HandleMessage(context.Background(), incoming("pageup", "/pageup"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/con") {
		t.Fatalf("changed panel reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("pagedown", "/pagedn"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/con") {
		t.Fatalf("page cache remained after change: %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceSplitsTerminalReplyAndStopsAfterIMFailure(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: oversizedTerminalText()}
	im := fakeIMFromService(t, service)
	beforeReplies, beforePushes := im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("split", "/con"))
	if im.replyCount() != beforeReplies+1 || im.pushCount() <= beforePushes {
		t.Fatalf("split deliveries replies=%#v pushes=%#v", im.replies, im.pushes)
	}
	for _, content := range append(append([]string(nil), im.replies...), im.pushes...) {
		if len(content) > panel.WeComContentLimit {
			t.Fatalf("content exceeds limit: %d", len(content))
		}
	}

	logs := &lockedLogBuffer{}
	service, fake = newTestServiceWithLogger(t, slog.New(slog.NewTextHandler(logs, nil)))
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: strings.Repeat("line\n", 6000)}
	im = fakeIMFromService(t, service)
	im.respondErr = errors.New("network response failed")
	_, beforePushes = im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("failed-reply", "/con"))
	if im.pushCount() != beforePushes {
		t.Fatalf("sent push after response failure: %#v", im.pushes)
	}
	output := logs.String()
	for _, want := range []string{
		"IM 回复发送失败", "request_hash=" + bridgeShortHash("request-failed-reply"),
		"part_index=1", "part_count=", "content_length=", "error_type=delivery",
		"reason=\"network response failed\"",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("首段回复失败日志缺少 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, "line\nline") {
		t.Fatalf("回复失败日志泄露终端内容：\n%s", output)
	}
}

func TestServiceStopsAfterFirstPushFailure(t *testing.T) {
	logs := &lockedLogBuffer{}
	service, fake := newTestServiceWithLogger(t, slog.New(slog.NewTextHandler(logs, nil)))
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: oversizedTerminalText()}
	im := fakeIMFromService(t, service)
	im.sendErr = errors.New("network push failed")
	_, beforePushes := im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("push-fail", "/con"))
	if got := im.pushCount() - beforePushes; got != 1 {
		t.Fatalf("push attempts = %d, want 1", got)
	}
	output := logs.String()
	for _, want := range []string{
		"IM 后续消息发送失败", "request_hash=" + bridgeShortHash("request-push-fail"),
		"part_index=2", "part_count=", "content_length=", "error_type=delivery",
		"reason=\"network push failed\"",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("后续分段失败日志缺少 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, "中文终端内容") {
		t.Fatalf("后续分段失败日志泄露终端内容：\n%s", output)
	}
}

func TestServiceInvalidateDoesNotWaitForBlockingIMReply(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	im := fakeIMFromService(t, service)
	im.blockRespond = make(chan struct{})
	im.respondStarted = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocking-reply", "prompt"))
		close(done)
	}()
	awaitSignal(t, im.respondStarted, "RespondMarkdown")
	invalidated := make(chan struct{})
	go func() { service.InvalidateSelection(); close(invalidated) }()
	awaitSignal(t, invalidated, "InvalidateSelection")
	close(im.blockRespond)
	awaitSignal(t, done, "HandleMessage")
	if len(fake.prompts()) != 1 {
		t.Fatalf("prompt calls = %#v", fake.prompts())
	}
}

func TestServiceSetNilWaitsForHerdrOperationAndThenBlocksNewOperations(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocked-prompt", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")
	setDone := make(chan struct{})
	go func() { service.SetHerdr(nil); close(setDone) }()
	assertNotClosed(t, setDone, "SetHerdr(nil) returned before Prompt")
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "prompt HandleMessage")
	awaitSignal(t, setDone, "SetHerdr(nil)")
	before := fake.callCount()
	service.HandleMessage(context.Background(), incoming("after-nil", "prompt"))
	service.HandleMessage(context.Background(), incoming("after-nil-key", "/key enter"))
	service.HandleMessage(context.Background(), incoming("after-nil-read", "/con"))
	if fake.callCount() != before {
		t.Fatalf("new operation called old client after nil: %#v", fake)
	}
}

func TestServiceConcurrentInvalidationsDoNotLetInputThrough(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptInFlight := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("multi-invalidate-in-flight", "prompt"))
		close(promptInFlight)
	}()
	awaitSignal(t, fake.promptStarted, "in-flight Prompt")
	firstHook, secondHook := make(chan struct{}, 1), make(chan struct{}, 1)
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	var hookMu sync.Mutex
	hookCount := 0
	service.beforeInvalidateStateChange = func() {
		hookMu.Lock()
		hookCount++
		count := hookCount
		hookMu.Unlock()
		if count == 1 {
			signal(firstHook)
			<-releaseFirst
			return
		}
		signal(secondHook)
		<-releaseSecond
	}
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() { service.InvalidateSelection(); close(firstDone) }()
	waitForCondition(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.inputBlocked > 0
	}, "first invalidation input barrier")
	go func() { service.InvalidateSelection(); close(secondDone) }()
	assertNotClosed(t, firstDone, "first InvalidateSelection returned before Prompt")
	close(fake.blockPrompt)
	awaitSignal(t, promptInFlight, "in-flight prompt")
	awaitSignal(t, firstHook, "first invalidation hook")
	close(releaseFirst)
	awaitSignal(t, firstDone, "first InvalidateSelection")
	awaitSignal(t, secondHook, "second invalidation hook")
	service.registry.CreateListSnapshot()
	if _, err := service.registry.Select(1); err != nil {
		t.Fatalf("failed to reconstruct test selection: %v", err)
	}
	before := fake.callCount()
	promptDone, keyDone := make(chan struct{}), make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("multi-invalidate-prompt", "prompt"))
		close(promptDone)
	}()
	go func() {
		service.HandleMessage(context.Background(), incoming("multi-invalidate-key", "/key enter"))
		close(keyDone)
	}()
	awaitSignal(t, promptDone, "blocked prompt")
	awaitSignal(t, keyDone, "blocked key")
	if fake.callCount() != before {
		t.Fatalf("input passed through pending second invalidation: %#v", fake)
	}
	close(releaseSecond)
	awaitSignal(t, secondDone, "second InvalidateSelection")
	if _, err := service.registry.ValidateSelected(); err == nil {
		t.Fatal("second invalidation did not clear the reconstructed selection")
	}
}

func TestServiceInvalidateAndSetNilWaitForBlockingGetAgent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockGet = make(chan struct{})
	fake.getStarted = make(chan struct{}, 1)
	inputDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocked-get", "prompt"))
		close(inputDone)
	}()
	awaitSignal(t, fake.getStarted, "GetAgent")
	invalidated := make(chan struct{})
	setDone := make(chan struct{})
	go func() { service.InvalidateSelection(); close(invalidated) }()
	go func() { service.SetHerdr(nil); close(setDone) }()
	assertNotClosed(t, invalidated, "InvalidateSelection returned before GetAgent")
	assertNotClosed(t, setDone, "SetHerdr(nil) returned before GetAgent")
	close(fake.blockGet)
	awaitSignal(t, inputDone, "input")
	awaitSignal(t, invalidated, "InvalidateSelection")
	awaitSignal(t, setDone, "SetHerdr(nil)")
}

func TestServiceSelectWaitsForInFlightPrompt(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := twoTargetSnapshot()
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-one", "/sel 1"))
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("select-blocked", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")
	fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: "SECOND-PANEL"}, nil)
	selectDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("select-two", "/sel 2"))
		close(selectDone)
	}()
	assertNotClosed(t, selectDone, "/sel returned before Prompt")
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "prompt")
	awaitSignal(t, selectDone, "/sel")
	second := agentInfo("pane-2", "terminal-2", "claude")
	second.DisplayAgent = stringRef("Claude")
	fake.setAgent(second)
	fake.promptResult = changedAgent(second, herdr.AgentStatusWorking)
	service.HandleMessage(context.Background(), incoming("select-two-prompt", "prompt"))
	got := fake.prompts()
	if len(got) != 2 || got[1].target != "pane-2" {
		t.Fatalf("selection target calls = %#v", got)
	}
}

func TestServiceConcurrentSelectAndPageDownNeverMixTargetAndPanel(t *testing.T) {
	service, fake := newTestService(t)
	snapshot := twoTargetSnapshot()
	service.registry.Replace(snapshot, false)
	fake.setSnapshot(snapshot)
	service.HandleMessage(context.Background(), incoming("mix-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("mix-select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("mix-content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("mix-pageup", "/pageup"))
	im := fakeIMFromService(t, service)
	beforeReplies := im.replyCount()
	hookEntered, releaseHook := make(chan struct{}, 1), make(chan struct{})
	service.beforePageDownReply = func() {
		signal(hookEntered)
		<-releaseHook
	}
	pageDone, selectDone := make(chan struct{}), make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("mix-pagedown", "/pagedn"))
		close(pageDone)
	}()
	awaitSignal(t, hookEntered, "/pagedn hook")
	fake.setRead(herdr.ReadResult{PaneID: "pane-2", Text: "SECOND-PANEL"}, nil)
	go func() {
		service.HandleMessage(context.Background(), incoming("mix-select-two", "/sel 2"))
		close(selectDone)
	}()
	assertNotClosed(t, selectDone, "/sel completed before /pagedn released state")
	close(releaseHook)
	awaitSignal(t, pageDone, "/pagedn")
	awaitSignal(t, selectDone, "/sel")
	for _, reply := range im.repliesFrom(beforeReplies) {
		if strings.Contains(reply, "line-100") && strings.Contains(reply, "Claude（pane-2）") {
			t.Fatalf("new target title was combined with old panel: %q", reply)
		}
	}
}

func TestServiceConcurrentDuplicateAndStateChangesAreRaceSafe(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	message := incoming("concurrent", "prompt")
	var group sync.WaitGroup
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			service.HandleMessage(context.Background(), message)
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			service.SetHerdr(fake)
			service.InvalidateSelection()
		}()
	}
	group.Wait()
	if got := len(fake.prompts()); got > 1 {
		t.Fatalf("duplicate calls sent %d prompts", got)
	}
}

func newTestService(t *testing.T) (*Service, *fakeHerdr) {
	return newTestServiceWithLogger(t, discardTestLogger())
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServiceWithLogger(t *testing.T, logger *slog.Logger) (*Service, *fakeHerdr) {
	t.Helper()
	registry := &session.Registry{}
	registry.Replace(testSnapshot(), false)
	guard, err := policy.NewGuard("user-1")
	if err != nil {
		t.Fatal(err)
	}
	deduper, err := policy.NewDeduper(time.Hour, 1000, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	current := agentInfo("pane-1", "terminal-1", "codex")
	fake := &fakeHerdr{
		snapshot: testSnapshot(), agent: current, promptResult: changedAgent(current, herdr.AgentStatusWorking),
		read: herdr.ReadResult{PaneID: "pane-1"},
	}
	im := &fakeIM{}
	audit := &fakeKeyAuditSink{}
	service, err := NewService(registry, &panel.Buffer{}, guard, deduper, im, audit, logger)
	if err != nil {
		t.Fatal(err)
	}
	service.waitKeyReadback = func(context.Context, time.Duration) error { return nil }
	service.SetHerdr(fake)
	return service, fake
}

func fakeAuditFromService(t *testing.T, service *Service) *fakeKeyAuditSink {
	t.Helper()
	audit, ok := service.keyAudit.(*fakeKeyAuditSink)
	if !ok {
		t.Fatalf("service key audit sink = %T, want *fakeKeyAuditSink", service.keyAudit)
	}
	return audit
}

func selectTarget(t *testing.T, service *Service) {
	t.Helper()
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-target", "/sel 1"))
	if fake, ok := service.client.Load().client.(*fakeHerdr); ok {
		fake.clearReads()
	}
}

func incoming(messageID, content string) wecom.IncomingText {
	return wecom.IncomingText{RequestID: "request-" + messageID, MessageID: messageID, UserID: "user-1", ChatType: "single", Content: content}
}

func testSnapshot() herdr.Snapshot {
	return herdr.Snapshot{
		Protocol:   herdr.RequiredProtocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "workspace-1"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "tab-1"}},
		Panes:      []herdr.Pane{{PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("codex"), DisplayAgent: stringRef("Codex"), AgentStatus: herdr.AgentStatusWorking}},
	}
}

func testSnapshotWithSession(value string) herdr.Snapshot {
	snapshot := testSnapshot()
	snapshot.Panes[0].AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: value}
	return snapshot
}

func twoTargetSnapshot() herdr.Snapshot {
	snapshot := testSnapshot()
	snapshot.Panes = append(snapshot.Panes, herdr.Pane{PaneID: "pane-2", TerminalID: "terminal-2", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), Title: stringRef("second"), AgentStatus: herdr.AgentStatusIdle})
	return snapshot
}

func agentInfo(paneID, terminalID, agent string) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: paneID, TerminalID: terminalID, Agent: stringRef(agent), DisplayAgent: stringRef("Codex"), AgentStatus: herdr.AgentStatusIdle, StateChangeSeq: 1}
}

func changedAgent(agent herdr.AgentInfo, status herdr.AgentStatus) herdr.AgentInfo {
	agent.AgentStatus = status
	agent.StateChangeSeq++
	return agent
}

func stringRef(value string) *string { return &value }

func textLines(start, end int) string {
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, fmt.Sprintf("line-%03d", index))
	}
	return strings.Join(lines, "\n")
}

func oversizedTerminalText() string {
	line := strings.Repeat("中文终端内容", 120)
	return strings.Repeat(line+"\n", panel.PageSize)
}

func namedLines(prefix string, start, end int) string {
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, fmt.Sprintf("%s-%03d", prefix, index))
	}
	return strings.Join(lines, "\n")
}

func replacedSnapshot() herdr.Snapshot {
	snapshot := testSnapshot()
	snapshot.Panes[0] = herdr.Pane{PaneID: "pane-1", TerminalID: "terminal-replaced", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), AgentStatus: herdr.AgentStatusWorking}
	return snapshot
}

type promptCall struct{ target, text string }
type keyCall struct{ target, key string }
type waitCall struct {
	target   string
	baseline uint64
	timeout  time.Duration
}

type agentGetResult struct {
	agent herdr.AgentInfo
	err   error
}
type readCall struct {
	target string
	lines  int
}

type fakeHerdr struct {
	mu            sync.Mutex
	snapshot      herdr.Snapshot
	snapshotErr   error
	snapshotCalls int
	agent         herdr.AgentInfo
	getErr        error
	getResults    []agentGetResult
	read          herdr.ReadResult
	readErr       error
	promptErr     error
	promptResult  herdr.AgentInfo
	waitErr       error
	waitResult    herdr.AgentInfo
	keyErr        error
	keyErrors     []error
	getCalls      []string
	readCalls     []readCall
	promptCalls   []promptCall
	waitCalls     []waitCall
	keyCalls      []keyCall
	order         []string
	blockGet      chan struct{}
	blockRead     chan struct{}
	blockPrompt   chan struct{}
	blockKey      chan struct{}
	getStarted    chan struct{}
	readStarted   chan struct{}
	promptStarted chan struct{}
	keyStarted    chan struct{}
}

type fakeKeyAuditSink struct {
	mu     sync.Mutex
	audits []policy.KeyAudit
}

func (s *fakeKeyAuditSink) RecordKeyAudit(audit policy.KeyAudit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, audit)
}

func (s *fakeKeyAuditSink) records() []policy.KeyAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]policy.KeyAudit(nil), s.audits...)
}

func (f *fakeHerdr) GetAgent(_ context.Context, target string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, target)
	f.order = append(f.order, "get")
	agent, err, block, started := f.agent, f.getErr, f.blockGet, f.getStarted
	if len(f.getResults) > 0 {
		agent, err = f.getResults[0].agent, f.getResults[0].err
		f.getResults = f.getResults[1:]
	}
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return agent, err
}
func (f *fakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	return f.snapshot, f.snapshotErr
}
func (f *fakeHerdr) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	f.mu.Lock()
	f.readCalls = append(f.readCalls, readCall{target, lines})
	f.order = append(f.order, "read")
	result, err, block, started := f.read, f.readErr, f.blockRead, f.readStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return result, err
}

func (f *fakeHerdr) ReadRecentANSI(ctx context.Context, target string, lines int) (herdr.ReadResult, error) {
	return f.ReadRecent(ctx, target, lines)
}
func (f *fakeHerdr) PromptUntilStateChange(_ context.Context, target, text string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	f.promptCalls = append(f.promptCalls, promptCall{target, text})
	f.order = append(f.order, "prompt")
	result, err, block, started := f.promptResult, f.promptErr, f.blockPrompt, f.promptStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return result, err
}
func (f *fakeHerdr) WaitForStateChange(_ context.Context, target string, baseline uint64, timeout time.Duration) (herdr.AgentInfo, error) {
	f.mu.Lock()
	f.waitCalls = append(f.waitCalls, waitCall{target: target, baseline: baseline, timeout: timeout})
	f.order = append(f.order, "wait")
	result, err := f.waitResult, f.waitErr
	f.mu.Unlock()
	return result, err
}
func (f *fakeHerdr) SendKey(_ context.Context, target, key string) error {
	f.mu.Lock()
	f.keyCalls = append(f.keyCalls, keyCall{target, key})
	f.order = append(f.order, "key")
	err, block, started := f.keyErr, f.blockKey, f.keyStarted
	if len(f.keyErrors) > 0 {
		err = f.keyErrors[0]
		f.keyErrors = f.keyErrors[1:]
	}
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return err
}
func (f *fakeHerdr) setAgent(agent herdr.AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agent = agent
}
func (f *fakeHerdr) setSnapshot(snapshot herdr.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = snapshot
}
func (f *fakeHerdr) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls
}
func (f *fakeHerdr) currentAgent() herdr.AgentInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agent
}
func (f *fakeHerdr) setGetResults(results ...agentGetResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getResults = append([]agentGetResult(nil), results...)
}
func (f *fakeHerdr) setRead(result herdr.ReadResult, block chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = result
	f.blockRead = block
}
func (f *fakeHerdr) prompts() []promptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]promptCall(nil), f.promptCalls...)
}
func (f *fakeHerdr) keys() []keyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]keyCall(nil), f.keyCalls...)
}
func (f *fakeHerdr) waits() []waitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]waitCall(nil), f.waitCalls...)
}
func (f *fakeHerdr) reads() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readCall(nil), f.readCalls...)
}
func (f *fakeHerdr) clearReads() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls = nil
	kept := f.order[:0]
	for _, call := range f.order {
		if call != "read" {
			kept = append(kept, call)
		}
	}
	f.order = kept
}
func (f *fakeHerdr) clearReadStarted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readStarted = nil
}
func (f *fakeHerdr) gets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getCalls...)
}
func (f *fakeHerdr) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}
func (f *fakeHerdr) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.getCalls) + len(f.readCalls) + len(f.promptCalls) + len(f.waitCalls) + len(f.keyCalls)
}

type fakeIM struct {
	mu             sync.Mutex
	replies        []string
	pushes         []string
	respondErr     error
	sendErr        error
	blockRespond   chan struct{}
	respondStarted chan struct{}
}

type terminalRecorder struct {
	mu      sync.Mutex
	replies []im.TerminalContent
	pushes  []im.TerminalContent
}

func (r *terminalRecorder) RespondMarkdown(context.Context, string, string) error { return nil }
func (r *terminalRecorder) SendMarkdown(context.Context, string) error            { return nil }
func (r *terminalRecorder) RespondTerminal(_ context.Context, _ string, content im.TerminalContent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, content)
	return nil
}
func (r *terminalRecorder) SendTerminal(_ context.Context, content im.TerminalContent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes = append(r.pushes, content)
	return nil
}
func (r *terminalRecorder) singleReply(t *testing.T) im.TerminalContent {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replies) != 1 {
		t.Fatalf("terminal replies = %#v", r.replies)
	}
	return r.replies[0]
}

type fakeTerminalRenderer struct {
	mu      sync.Mutex
	result  terminalimage.Result
	err     error
	ansi    []string
	started chan struct{}
	block   chan struct{}
}

func (r *fakeTerminalRenderer) Render(_ context.Context, safeANSI string) (terminalimage.Result, error) {
	r.mu.Lock()
	r.ansi = append(r.ansi, safeANSI)
	result, err, started, block := r.result, r.err, r.started, r.block
	r.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return result, err
}

func (r *fakeTerminalRenderer) lastANSI() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ansi) == 0 {
		return ""
	}
	return r.ansi[len(r.ansi)-1]
}

func (f *fakeIM) RespondMarkdown(_ context.Context, _ string, content string) error {
	f.mu.Lock()
	f.replies = append(f.replies, content)
	err, block, started := f.respondErr, f.blockRespond, f.respondStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return err
}
func (f *fakeIM) SendMarkdown(_ context.Context, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, content)
	return f.sendErr
}
func (f *fakeIM) replyCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.replies) }
func (f *fakeIM) repliesFrom(index int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.replies) {
		return nil
	}
	return append([]string(nil), f.replies[index:]...)
}
func (f *fakeIM) pushCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pushes) }
func (f *fakeIM) deliveryCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies), len(f.pushes)
}
func (f *fakeIM) lastReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) == 0 {
		return ""
	}
	return f.replies[len(f.replies)-1]
}

func fakeIMFromService(t *testing.T, service *Service) *fakeIM {
	t.Helper()
	im, ok := service.im.(*fakeIM)
	if !ok {
		t.Fatalf("service IM = %T, want *fakeIM", service.im)
	}
	return im
}

func signal(channel chan<- struct{}) {
	if channel != nil {
		channel <- struct{}{}
	}
}

func awaitSignal(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal(name)
	case <-time.After(30 * time.Millisecond):
	}
}

func awaitChangeSet(t *testing.T, channel <-chan session.ChangeSet, name string) session.ChangeSet {
	t.Helper()
	select {
	case changes := <-channel:
		return changes
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return session.ChangeSet{}
	}
}

func assertPanelState(t *testing.T, service *Service, wantReady bool, wantPage int, wantContent string) {
	t.Helper()
	service.stateMu.Lock()
	ready, page := service.panelReady, service.page
	content := strings.Join(service.panel.Render(), "\n")
	service.stateMu.Unlock()
	if ready != wantReady || page != wantPage {
		t.Fatalf("panel state = ready %t page %d, want ready %t page %d", ready, page, wantReady, wantPage)
	}
	if wantContent == "" {
		if content != "" {
			t.Fatalf("panel content = %q, want empty", content)
		}
		return
	}
	if !strings.Contains(content, wantContent) {
		t.Fatalf("panel content = %q, want %q", content, wantContent)
	}
}

func waitForCondition(t *testing.T, predicate func() bool, name string) {
	t.Helper()
	deadline := time.After(time.Second)
	for !predicate() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", name)
		default:
			runtime.Gosched()
		}
	}
}
