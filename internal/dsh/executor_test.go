package dsh

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// fakePromptCall 记录一次已提交的普通文本。
type fakePromptCall struct {
	sessionID string
	requestID string
	text      string
}

// fakeSubmitCall 记录一次已提交的决策回答。
type fakeSubmitCall struct {
	decision Decision
	value    any
}

// fakeExecutorAPI 是单宿主执行器测试替身；记录命令调用并返回可配置结果。
type fakeExecutorAPI struct {
	sessions    []SessionSummary
	pending     PendingSnapshot
	sessionsErr error
	pendingErr  error

	readText string
	readErr  error

	promptCalls []fakePromptCall
	promptErr   error

	createCalls  int
	createCwd    string
	createTitle  string
	createResult CreatedSession
	createErr    error

	decisions    []Decision
	decisionsErr error

	submitCalls   []fakeSubmitCall
	submitReceipt AnswerReceipt
	submitErr     error
}

func (f *fakeExecutorAPI) Sessions(context.Context) ([]SessionSummary, error) {
	return append([]SessionSummary(nil), f.sessions...), f.sessionsErr
}

func (f *fakeExecutorAPI) Pending(context.Context) (PendingSnapshot, error) {
	return f.pending, f.pendingErr
}

func (f *fakeExecutorAPI) Read(context.Context, string) (string, error) {
	return f.readText, f.readErr
}

func (f *fakeExecutorAPI) Prompt(_ context.Context, sessionID, requestID, text string) error {
	f.promptCalls = append(f.promptCalls, fakePromptCall{sessionID: sessionID, requestID: requestID, text: text})
	return f.promptErr
}

func (f *fakeExecutorAPI) Create(_ context.Context, cwd, title string) (CreatedSession, error) {
	f.createCalls++
	f.createCwd = cwd
	f.createTitle = title
	return f.createResult, f.createErr
}

func (f *fakeExecutorAPI) Decisions(context.Context, string) ([]Decision, error) {
	return append([]Decision(nil), f.decisions...), f.decisionsErr
}

func (f *fakeExecutorAPI) SubmitAnswer(_ context.Context, decision Decision, value any) (AnswerReceipt, error) {
	f.submitCalls = append(f.submitCalls, fakeSubmitCall{decision: decision, value: value})
	return f.submitReceipt, f.submitErr
}

// fakeExecutorSink 记录执行器发出的通知和命令回复。
type fakeExecutorSink struct {
	targets         []im.NotificationTarget
	events          []im.NotificationEvent
	replyRequestIDs []string
	replies         []string
}

func (s *fakeExecutorSink) RespondMarkdown(_ context.Context, requestID, content string) error {
	s.replyRequestIDs = append(s.replyRequestIDs, requestID)
	s.replies = append(s.replies, content)
	return nil
}

func (s *fakeExecutorSink) SendNotification(_ context.Context, target im.NotificationTarget, event im.NotificationEvent) error {
	s.targets = append(s.targets, target)
	s.events = append(s.events, event)
	return nil
}

// lastReply 返回执行器最近一次命令回复。
func (s *fakeExecutorSink) lastReply() string {
	if len(s.replies) == 0 {
		return ""
	}
	return s.replies[len(s.replies)-1]
}

// newExecutorFixture 创建使用 fake API 和 sink 的单宿主执行器。
func newExecutorFixture(t *testing.T, host Host) (*Executor, *fakeExecutorAPI, *fakeExecutorSink) {
	t.Helper()
	guard, err := policy.NewGuard("7")
	if err != nil {
		t.Fatalf("NewGuard() 返回错误：%v", err)
	}
	api := &fakeExecutorAPI{}
	sink := &fakeExecutorSink{}
	return NewExecutor(host, api, sink, guard), api, sink
}

// testExecutorShortHash 按设计文档的规则计算目标标识中的短摘要。
func testExecutorShortHash(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))[:16]
}

func TestExecutorListsOnlyAvailableSessionsAndHostEntry(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{
		{ID: "s1", Title: "修复登录", Cwd: "/Users/alice/work/herdr-pal", Available: true, UpdatedAt: 10},
		{ID: "hidden", Title: "不可控制会话", Cwd: "/tmp/hidden", Available: false, UpdatedAt: 20},
	}

	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("CurrentTargets() 数量 = %d, want 1（只应有可控制会话）：%#v", len(targets), targets)
	}
	got := targets[0]
	if !strings.HasPrefix(got.PaneID, "dsh:") || !strings.HasPrefix(got.OccupantKey, "session:") {
		t.Fatalf("会话目标标识格式错误：%#v", got)
	}
	if got.Agent != "dsh" || got.DisplayAgent != "DeepSeek Harness" {
		t.Fatalf("Agent 展示字段错误：%#v", got)
	}
	if got.Title != "修复登录" || got.Workspace != "herdr-pal" || got.Tab != "web" {
		t.Fatalf("标题/工作区/标签页错误：%#v", got)
	}
	if got.Status != herdr.AgentStatusIdle {
		t.Fatalf("Status = %q, want idle", got.Status)
	}

	api.sessions = []SessionSummary{{ID: "s2", Available: true}}
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("CurrentTargets() 数量 = %d, want 1", len(targets))
	}
	if targets[0].Title != "s2" {
		t.Fatalf("空标题应回退为会话 ID，got %q", targets[0].Title)
	}
	if targets[0].Workspace != "dsh" {
		t.Fatalf("空工作目录应展示 dsh，got %q", targets[0].Workspace)
	}

	api.sessions = nil
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("无会话时应只有宿主入口，got %#v", targets)
	}
	hostTarget := executor.hostTarget()
	if targets[0] != hostTarget {
		t.Fatalf("宿主入口 = %#v, want %#v", targets[0], hostTarget)
	}
	if targets[0].Title != "新建会话" || targets[0].Tab != "dsh-host" ||
		targets[0].Workspace != "web" || targets[0].Agent != "dsh" ||
		targets[0].DisplayAgent != "DeepSeek Harness" {
		t.Fatalf("宿主入口字段错误：%#v", targets[0])
	}
}

func TestExecutorSortsSessionsByTitleThenSessionID(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{
		{ID: "b", Title: "同一标题", Available: true, UpdatedAt: 30},
		{ID: "a", Title: "同一标题", Available: true, UpdatedAt: 20},
		{ID: "z", Title: "A 标题", Available: true, UpdatedAt: 10},
	}
	executor.refresh(context.Background())

	targets := executor.CurrentTargets()
	if len(targets) != 3 {
		t.Fatalf("CurrentTargets() 数量 = %d, want 3", len(targets))
	}
	gotIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		state, ok := executor.sessions[target.PaneID]
		if !ok {
			t.Fatalf("目标 %q 不在目录中", target.PaneID)
		}
		gotIDs = append(gotIDs, state.summary.ID)
	}
	if order := strings.Join(gotIDs, ","); order != "z,a,b" {
		t.Fatalf("会话排序 = %s, want z,a,b（标题升序，标题相同时会话 ID 升序）", order)
	}

	api.sessions = nil
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 || targets[0].Tab != hostEntryTab {
		t.Fatalf("无会话时宿主入口必须排在首位：%#v", targets)
	}
}

func TestExecutorTargetIDsIncludeHostIdentity(t *testing.T) {
	summary := SessionSummary{ID: "shared", Title: "共享历史", Available: true}

	hostWeb := testValidHost()
	executorWeb, apiWeb, _ := newExecutorFixture(t, hostWeb)
	apiWeb.sessions = []SessionSummary{summary}
	executorWeb.refresh(context.Background())
	webTarget := executorWeb.CurrentTargets()[0]

	hostDesktop := testValidHost()
	hostDesktop.Port = 3090
	hostDesktop.Variant = "desktop"
	executorDesktop, apiDesktop, _ := newExecutorFixture(t, hostDesktop)
	apiDesktop.sessions = []SessionSummary{summary}
	executorDesktop.refresh(context.Background())
	desktopTarget := executorDesktop.CurrentTargets()[0]

	hostRestarted := testValidHost()
	hostRestarted.Generation = "fedcba9876543210fedcba98"
	executorRestarted, apiRestarted, _ := newExecutorFixture(t, hostRestarted)
	apiRestarted.sessions = []SessionSummary{summary}
	executorRestarted.refresh(context.Background())
	restartedTarget := executorRestarted.CurrentTargets()[0]

	wantPaneID := "dsh:" + testExecutorShortHash(hostWeb.Reference(), summary.ID)
	wantOccupant := "session:" + testExecutorShortHash(hostWeb.Generation, summary.ID)
	if webTarget.PaneID != wantPaneID || webTarget.OccupantKey != wantOccupant {
		t.Fatalf("目标标识 = (%q, %q), want (%q, %q)", webTarget.PaneID, webTarget.OccupantKey, wantPaneID, wantOccupant)
	}
	if webTarget.PaneID == desktopTarget.PaneID {
		t.Fatal("不同宿主的同一会话 ID 必须生成不同 PaneID")
	}
	if webTarget.Tab != "web" || desktopTarget.Tab != "desktop" {
		t.Fatalf("Tab 应取宿主变体：web=%q desktop=%q", webTarget.Tab, desktopTarget.Tab)
	}
	if webTarget.PaneID == restartedTarget.PaneID {
		t.Fatal("宿主 Generation 变化后 PaneID 必须变化")
	}
	if webTarget.OccupantKey == restartedTarget.OccupantKey {
		t.Fatal("宿主 Generation 变化后 OccupantKey 必须变化")
	}
	if webTarget.OccupantKey != desktopTarget.OccupantKey {
		t.Fatal("同一 Generation 下 OccupantKey 只应取决于会话 ID")
	}

	wantHostSlot := "dsh-host:" + testExecutorShortHash(hostWeb.Reference())
	if got := executorWeb.hostTarget().PaneID; got != wantHostSlot {
		t.Fatalf("宿主入口 PaneID = %q, want %q", got, wantHostSlot)
	}
	if executorWeb.hostTarget().PaneID == executorDesktop.hostTarget().PaneID {
		t.Fatal("不同宿主的入口 PaneID 必须不同")
	}
}

func TestExecutorBlockedStatusAndNotification(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.pending = PendingSnapshot{ClientID: "client-1", Items: []PendingItem{{
		EventID: "e1", AgentID: "s1", Event: "approval/request",
		Request: json.RawMessage(`{"toolName":"Bash"}`),
	}}}

	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusBlocked {
		t.Fatalf("审批待答应进入 blocked：%#v", targets)
	}
	if len(sink.events) != 1 {
		t.Fatalf("首次 blocked 必须通知一次，got %d", len(sink.events))
	}
	event := sink.events[0]
	if event.Kind != im.NotificationKindAgentStatusChanged ||
		event.PreviousStatus != "unknown" || event.Status != "blocked" {
		t.Fatalf("首次 blocked 通知 = %#v", event)
	}
	if sink.targets[0].PaneID != targets[0].PaneID ||
		sink.targets[0].OccupantHash != targets[0].OccupantKey ||
		sink.targets[0].Title != "任务" || sink.targets[0].Agent != "dsh" ||
		sink.targets[0].DisplayAgent != "DeepSeek Harness" {
		t.Fatalf("通知目标错误：%#v", sink.targets[0])
	}

	executor.refresh(context.Background())
	if len(sink.events) != 1 {
		t.Fatalf("相同 blocked 状态不应重复通知，got %d", len(sink.events))
	}

	// 定时问题允许原生界面先行超时，不进入 blocked；此时 running 决定为 working。
	api.sessions[0].Running = true
	api.pending = PendingSnapshot{Items: []PendingItem{{
		EventID: "e2", AgentID: "s1", Event: "user-questions/request",
		Request: json.RawMessage(`{"wait":{"timed":true}}`),
	}}}
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusWorking {
		t.Fatalf("定时问题不应 blocking，running 应为 working：%#v", targets)
	}
	if len(sink.events) != 2 || sink.events[1].PreviousStatus != "blocked" || sink.events[1].Status != "working" {
		t.Fatalf("blocked -> working 通知错误：%#v", sink.events)
	}

	// 非定时问题在 running 时也应优先进入 blocked。
	api.pending.Items[0].Request = json.RawMessage(`{"wait":{"callId":"call-1"}}`)
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusBlocked {
		t.Fatalf("非定时问题应 blocking：%#v", targets)
	}
	if len(sink.events) != 3 || sink.events[2].PreviousStatus != "working" || sink.events[2].Status != "blocked" {
		t.Fatalf("working -> blocked 通知错误：%#v", sink.events)
	}
}

func TestExecutorOfflineWithdrawsTargets(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("初始目录数量 = %d, want 1", len(targets))
	}
	target := targets[0]
	if err := executor.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatalf("SelectTarget() 返回错误：%v", err)
	}

	api.sessionsErr = errors.New("宿主不可用")
	executor.refresh(context.Background())
	if targets := executor.CurrentTargets(); len(targets) != 0 {
		t.Fatalf("API 错误后目录必须清空（含宿主入口），got %#v", targets)
	}
	if _, err := executor.SelectedTarget(); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("API 错误后 SelectedTarget() = %v, want ErrSelectionInvalid", err)
	}
	if err := executor.SelectTarget(target.PaneID, target.OccupantKey); err == nil {
		t.Fatal("API 错误后旧目标不应可选")
	}

	// 恢复目录但 Pending 单独失败：只记录日志，不撤下目录。
	api.sessionsErr = nil
	api.pendingErr = errors.New("Mux 不可用")
	executor.refresh(context.Background())
	targets = executor.CurrentTargets()
	if len(targets) != 1 {
		t.Fatalf("Pending 失败不应撤下目录，got %#v", targets)
	}
	if targets[0].Status != herdr.AgentStatusIdle {
		t.Fatalf("无待答信息时按 running 推导状态，got %q", targets[0].Status)
	}
	if _, err := executor.SelectedTarget(); err != nil {
		t.Fatalf("同一身份恢复后选择应重新可用：%v", err)
	}
}

func TestExecutorSelectionRejectsStaleOccupant(t *testing.T) {
	host := testValidHost()
	executor, api, _ := newExecutorFixture(t, host)
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	executor.refresh(context.Background())
	target := executor.CurrentTargets()[0]

	if err := executor.SelectTarget(target.PaneID, "stale"); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("错误 occupant 应返回 ErrSelectionInvalid，got %v", err)
	}
	if err := executor.SelectTarget("dsh:missing", target.OccupantKey); err == nil {
		t.Fatal("未知 PaneID 不应可选")
	}
	if err := executor.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatalf("SelectTarget() 返回错误：%v", err)
	}
	selected, err := executor.SelectedTarget()
	if err != nil || selected != target {
		t.Fatalf("SelectedTarget() = (%#v, %v), want (%#v, nil)", selected, err, target)
	}

	api.sessions = nil
	executor.refresh(context.Background())
	if _, err := executor.SelectedTarget(); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("会话消失后 SelectedTarget() = %v, want ErrSelectionInvalid", err)
	}
	if err := executor.SelectTarget(target.PaneID, target.OccupantKey); err == nil {
		t.Fatal("会话消失后旧 occupant 不应可选")
	}
	hostTarget := executor.hostTarget()
	if err := executor.SelectTarget(hostTarget.PaneID, hostTarget.OccupantKey); err != nil {
		t.Fatalf("无会话时宿主入口应可选：%v", err)
	}

	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	executor.refresh(context.Background())
	if _, err := executor.SelectedTarget(); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("会话恢复后宿主入口选择应失效，got %v", err)
	}

	// 宿主 Generation 变化后，旧 PaneID 与旧 occupant 都不能复用。
	restartedHost := host
	restartedHost.Generation = "fedcba9876543210fedcba98"
	restarted, restartedAPI, _ := newExecutorFixture(t, restartedHost)
	restartedAPI.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	restarted.refresh(context.Background())
	restartedTarget := restarted.CurrentTargets()[0]
	if restartedTarget.PaneID == target.PaneID || restartedTarget.OccupantKey == target.OccupantKey {
		t.Fatalf("宿主重启后目标身份必须变化：old=%#v new=%#v", target, restartedTarget)
	}
	for _, identity := range [][2]string{
		{target.PaneID, target.OccupantKey},
		{restartedTarget.PaneID, target.OccupantKey},
		{target.PaneID, restartedTarget.OccupantKey},
	} {
		if err := restarted.SelectTarget(identity[0], identity[1]); err == nil {
			t.Fatalf("过期身份 %v 不应可选", identity)
		}
	}
	if err := restarted.SelectTarget(restartedTarget.PaneID, restartedTarget.OccupantKey); err != nil {
		t.Fatalf("新身份应可选：%v", err)
	}
}

func TestExecutorNotifiesOncePerTransition(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true, UpdatedAt: 100}}

	refresh := func() { executor.refresh(context.Background()) }
	refresh()
	if len(sink.events) != 0 {
		t.Fatalf("首次 idle 不应通知，got %#v", sink.events)
	}

	api.sessions[0].Running = true
	refresh()
	refresh()
	if len(sink.events) != 1 {
		t.Fatalf("idle -> working 应只通知一次，got %d", len(sink.events))
	}

	api.sessions[0].Running = false
	refresh()
	refresh()
	if len(sink.events) != 2 {
		t.Fatalf("working -> done 应只通知一次，got %d", len(sink.events))
	}

	api.sessions[0].UpdatedAt = 200
	refresh()
	refresh()
	if len(sink.events) != 3 {
		t.Fatalf("idle UpdatedAt 变化应再通知一次，got %d", len(sink.events))
	}

	api.pending.Items = []PendingItem{{EventID: "e1", AgentID: "s1", Event: "approval/request"}}
	api.sessions[0].UpdatedAt = 300
	refresh()
	refresh()
	if len(sink.events) != 4 {
		t.Fatalf("idle -> blocked 应只通知一次，got %d", len(sink.events))
	}

	// 同一 blocked 状态下决策身份变化也必须通知。
	api.pending.Items = []PendingItem{{EventID: "e2", AgentID: "s1", Event: "approval/request"}}
	api.sessions[0].UpdatedAt = 400
	refresh()
	refresh()
	if len(sink.events) != 5 {
		t.Fatalf("新 blocked 决策应再通知一次，got %d", len(sink.events))
	}

	api.pending.Items = nil
	api.sessions[0].UpdatedAt = 500
	refresh()
	refresh()
	if len(sink.events) != 6 {
		t.Fatalf("blocked -> done 应只通知一次，got %d", len(sink.events))
	}

	want := []struct {
		previous string
		status   string
	}{
		{"idle", "working"},
		{"working", "done"},
		{"idle", "done"},
		{"idle", "blocked"},
		{"unknown", "blocked"},
		{"blocked", "done"},
	}
	if len(sink.events) != len(want) {
		t.Fatalf("通知数量 = %d, want %d", len(sink.events), len(want))
	}
	for index, expected := range want {
		event := sink.events[index]
		if event.PreviousStatus != expected.previous || event.Status != expected.status {
			t.Fatalf("通知[%d] = (%q -> %q), want (%q -> %q)",
				index, event.PreviousStatus, event.Status, expected.previous, expected.status)
		}
		if event.Kind != im.NotificationKindAgentStatusChanged || event.OccurredAt.IsZero() {
			t.Fatalf("通知[%d] 元数据错误：%#v", index, event)
		}
	}
}

// executorIncoming 构造一条已通过 IM 协议校验的测试消息。
func executorIncoming(messageID, content string) im.IncomingText {
	return im.IncomingText{
		RequestID: "request-" + messageID,
		MessageID: messageID,
		BotID:     "bot-1",
		UserID:    "7",
		ChatType:  "single",
		Content:   content,
	}
}

// selectExecutorSession 刷新目录并选择指定会话，返回对应目标。
func selectExecutorSession(t *testing.T, executor *Executor, api *fakeExecutorAPI, sessionID string) session.Target {
	t.Helper()
	executor.refresh(context.Background())
	for _, target := range executor.CurrentTargets() {
		state, ok := executor.sessions[target.PaneID]
		if !ok || state.summary.ID != sessionID {
			continue
		}
		if err := executor.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
			t.Fatalf("SelectTarget() 返回错误：%v", err)
		}
		return target
	}
	t.Fatalf("目录中没有会话 %q：api.sessions=%#v", sessionID, api.sessions)
	return session.Target{}
}

// selectExecutorHostEntry 刷新目录并选择宿主入口。
func selectExecutorHostEntry(t *testing.T, executor *Executor) session.Target {
	t.Helper()
	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) != 1 || targets[0].Tab != hostEntryTab {
		t.Fatalf("期望只有宿主入口，got %#v", targets)
	}
	if err := executor.SelectTarget(targets[0].PaneID, targets[0].OccupantKey); err != nil {
		t.Fatalf("SelectTarget() 返回错误：%v", err)
	}
	return targets[0]
}

// testApprovalDecision 构造一条 blocking 审批决策。
func testApprovalDecision(sessionID, eventID, revision string) Decision {
	return Decision{
		NativeID:  "event:" + eventID,
		SessionID: sessionID,
		Kind:      decisionKindApproval,
		Title:     "审批：Bash",
		Questions: []Question{{
			ID:          "approval",
			Title:       "是否允许本次工具调用？",
			Description: "运行测试命令",
			Kind:        questionKindApproval,
			Options:     approvalOptions(),
			Required:    true,
		}},
		Revision:  revision,
		Blocking:  true,
		Supported: true,
	}
}

// testTwoQuestionForm 构造一条包含单选和文本题的表单决策。
func testTwoQuestionForm(sessionID, revision string) Decision {
	return Decision{
		NativeID:  "event:e1",
		SessionID: sessionID,
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Questions: []Question{
			{
				ID:       "q1",
				Title:    "第一题",
				Kind:     questionKindSingle,
				Options:  []Option{{Value: "A", Label: "选项 A"}, {Value: "B", Label: "选项 B"}},
				Required: true,
			},
			{ID: "q2", Title: "第二题", Kind: questionKindText, Required: true},
		},
		Revision:  revision,
		Blocking:  true,
		Supported: true,
	}
}

func TestExecutorPromptQueuesWhileRunning(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true, Running: true}}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("msg-1", "继续处理"))

	if len(api.promptCalls) != 1 {
		t.Fatalf("Prompt 调用次数 = %d, want 1", len(api.promptCalls))
	}
	call := api.promptCalls[0]
	if call.sessionID != "s1" || call.requestID != "msg-1" || call.text != "继续处理" {
		t.Fatalf("Prompt 调用参数错误：%#v", call)
	}
	reply := sink.lastReply()
	if !strings.Contains(reply, "已发送") {
		t.Fatalf("回复 = %q, want 包含“已发送”", reply)
	}
	if len(sink.replyRequestIDs) != 1 || sink.replyRequestIDs[0] != "request-msg-1" {
		t.Fatalf("回复请求标识错误：%#v", sink.replyRequestIDs)
	}
}

func TestExecutorPromptRejectedWhenBlocked(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.pending = PendingSnapshot{ClientID: "client-1", Items: []PendingItem{{
		EventID: "e1", AgentID: "s1", Event: "approval/request",
		Request: json.RawMessage(`{"toolName":"Bash"}`),
	}}}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("msg-blocked", "放行"))

	if len(api.promptCalls) != 0 {
		t.Fatalf("blocked 时不应调用 Prompt，got %#v", api.promptCalls)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "/answer") {
		t.Fatalf("回复 = %q, want 提示先使用 /answer", reply)
	}
}

func TestExecutorReadAppendsPendingApproval(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.readText = "用户：你好\n\n助手：请确认"
	api.decisions = []Decision{testApprovalDecision("s1", "e1", "rev-1")}
	target := selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("msg-con", "/con"))

	reply := sink.lastReply()
	if !strings.Contains(reply, "用户：你好") || !strings.Contains(reply, "助手：请确认") {
		t.Fatalf("回复缺少对话正文：%q", reply)
	}
	if !strings.Contains(reply, "1. 允许本次") || !strings.Contains(reply, "/answer") {
		t.Fatalf("回复缺少审批提示：%q", reply)
	}

	content, err := executor.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil {
		t.Fatalf("ReadTerminalSnapshot() 返回错误：%v", err)
	}
	if content.Mode != im.OutputModeText || !strings.Contains(content.Text, "1. 允许本次") || !strings.Contains(content.Text, "/answer") {
		t.Fatalf("ReadTerminalSnapshot() = %#v", content)
	}
	if content.Page == nil || content.Page.Current != 1 || content.Page.Total != 1 {
		t.Fatalf("ReadTerminalSnapshot() 页码错误：%#v", content.Page)
	}
}

func TestExecutorReadTerminalSnapshotBoundsTextAndKeepsPrompt(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.readText = strings.Repeat("正", 25000)
	api.decisions = []Decision{testApprovalDecision("s1", "e1", "rev-1")}
	target := selectExecutorSession(t, executor, api, "s1")

	content, err := executor.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil {
		t.Fatalf("ReadTerminalSnapshot() 返回错误：%v", err)
	}
	if length := utf16Length(content.Text); length > 20000 {
		t.Fatalf("快照 UTF-16 长度 = %d, want <= 20000", length)
	}
	if !strings.Contains(content.Text, "1. 允许本次") || !strings.Contains(content.Text, "/answer") {
		t.Fatalf("截断后必须保留待答提示：%q", content.Text)
	}
	if strings.Count(content.Text, "正") >= 25000 {
		t.Fatal("超长正文未被截断")
	}

	api.readText = "第一行\n第二行\n第三行"
	api.decisions = nil
	content, err = executor.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, 2)
	if err != nil {
		t.Fatalf("ReadTerminalSnapshot() 返回错误：%v", err)
	}
	if content.Text != "第二行\n第三行" {
		t.Fatalf("maxLines 截断结果 = %q, want 最后两行", content.Text)
	}
}

func TestExecutorAnswerCollectsMultipleQuestions(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.decisions = []Decision{testTwoQuestionForm("s1", "rev-1")}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("answer-1", "/answer 2"))

	if len(api.submitCalls) != 0 {
		t.Fatalf("第一题回答后不应提交，got %#v", api.submitCalls)
	}
	if len(executor.answerStates) != 1 {
		t.Fatalf("应保留未完成的答案状态，got %d", len(executor.answerStates))
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "第二题") || !strings.Contains(reply, "/answer") {
		t.Fatalf("第一题回复应包含下一题：%q", reply)
	}

	executor.HandleMessage(context.Background(), executorIncoming("answer-2", "/answer 自定义答案"))

	if len(api.submitCalls) != 1 {
		t.Fatalf("最后一题回答后应只提交一次，got %d", len(api.submitCalls))
	}
	call := api.submitCalls[0]
	if call.decision.NativeID != "event:e1" {
		t.Fatalf("提交的决策 = %q, want event:e1", call.decision.NativeID)
	}
	value, ok := call.value.(answerValue)
	if !ok {
		t.Fatalf("提交值类型 = %T, want answerValue", call.value)
	}
	if len(value.Answers) != 2 {
		t.Fatalf("提交答案数 = %d, want 2", len(value.Answers))
	}
	if value.Answers[0].ID != "q1" || len(value.Answers[0].Selected) != 1 || value.Answers[0].Selected[0] != "B" {
		t.Fatalf("第一题提交答案错误：%#v", value.Answers[0])
	}
	if value.Answers[1].ID != "q2" || value.Answers[1].Custom != "自定义答案" || len(value.Answers[1].Selected) != 0 {
		t.Fatalf("第二题提交答案错误：%#v", value.Answers[1])
	}
	if len(executor.answerStates) != 0 {
		t.Fatalf("提交后必须清空答案状态，got %d", len(executor.answerStates))
	}
}

func TestExecutorAnswerFallsBackToContinuedQuestion(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.decisions = []Decision{{
		NativeID:  "call:c1",
		SessionID: "s1",
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Questions: []Question{{ID: "q1", Title: "补充说明", Kind: questionKindText, Required: true}},
		Revision:  "rev-call",
		Blocking:  false,
		Supported: true,
	}}
	api.submitReceipt = AnswerReceipt{Confirmed: true, Message: "已回答，dsh 已接收。"}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("answer-call", "/answer 这是答案"))

	if len(api.submitCalls) != 1 || api.submitCalls[0].decision.NativeID != "call:c1" {
		t.Fatalf("持续问题应可回答，got %#v", api.submitCalls)
	}
	value, ok := api.submitCalls[0].value.(answerValue)
	if !ok || len(value.Answers) != 1 || value.Answers[0].Custom != "这是答案" {
		t.Fatalf("持续问题提交值错误：%#v", api.submitCalls[0].value)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "已回答") {
		t.Fatalf("回复 = %q, want 包含回执", reply)
	}
}

func TestExecutorTimedWaterfallQuestionShownAndAnswerable(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	timed := Decision{
		NativeID:  "event:timed-1",
		SessionID: "s1",
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Questions: []Question{{
			ID:       "tq",
			Title:    "定时问题",
			Kind:     questionKindSingle,
			Options:  []Option{{Value: "X", Label: "选项 X"}, {Value: "Y", Label: "选项 Y"}},
			Required: true,
		}},
		Revision:  "rev-timed",
		Blocking:  false,
		Supported: true,
	}
	continued := Decision{
		NativeID:  "call:c-cont",
		SessionID: "s1",
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Questions: []Question{{ID: "cq", Title: "持续问题", Kind: questionKindText, Required: true}},
		Revision:  "rev-cont",
		Blocking:  false,
		Supported: true,
	}
	// 决策顺序与 Client.Decisions 一致：waterfall 在前，投影持续问题在后。
	api.decisions = []Decision{timed, continued}
	api.readText = "用户：开始"
	api.submitReceipt = AnswerReceipt{Confirmed: true, Message: "已提交，dsh 已确认。"}
	target := selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("timed-con", "/con"))
	reply := sink.lastReply()
	if !strings.Contains(reply, "定时问题") || !strings.Contains(reply, "1. 选项 X") || !strings.Contains(reply, "/answer") {
		t.Fatalf("/con 未展示定时 waterfall 问题：%q", reply)
	}
	if strings.Contains(reply, "持续问题") {
		t.Fatalf("/con 应选择决策顺序中的第一个 supported 决策：%q", reply)
	}

	content, err := executor.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil {
		t.Fatalf("ReadTerminalSnapshot() 返回错误：%v", err)
	}
	if !strings.Contains(content.Text, "定时问题") || strings.Contains(content.Text, "持续问题") {
		t.Fatalf("ReadTerminalSnapshot() 未按同一规则展示定时问题：%q", content.Text)
	}

	executor.HandleMessage(context.Background(), executorIncoming("timed-answer", "/answer 2"))
	if len(api.submitCalls) != 1 {
		t.Fatalf("定时 waterfall 问题应可回答，got %#v", api.submitCalls)
	}
	call := api.submitCalls[0]
	// event: 前缀由 Client.SubmitAnswer 路由到 $events/result；call: 前缀才走
	// userQuestions/answer（见 answers_test.go 的端点测试）。
	if call.decision.NativeID != "event:timed-1" || call.decision.Kind != decisionKindForm {
		t.Fatalf("提交决策 = %#v, want event:timed-1 form", call.decision)
	}
	value, ok := call.value.(answerValue)
	if !ok || len(value.Answers) != 1 || len(value.Answers[0].Selected) != 1 || value.Answers[0].Selected[0] != "Y" {
		t.Fatalf("定时问题提交值错误：%#v", call.value)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "已提交") {
		t.Fatalf("回复 = %q, want 提交回执", reply)
	}
}

func TestExecutorAnswerRejectsStaleRevision(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.decisions = []Decision{testTwoQuestionForm("s1", "rev-1")}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("stale-1", "/answer 1"))
	if len(executor.answerStates) != 1 {
		t.Fatalf("应保留未完成答案状态，got %d", len(executor.answerStates))
	}

	api.decisions[0].Revision = "rev-2"
	executor.HandleMessage(context.Background(), executorIncoming("stale-2", "/answer 自定义"))

	if len(api.submitCalls) != 0 {
		t.Fatalf("revision 变化后不应提交，got %#v", api.submitCalls)
	}
	if len(executor.answerStates) != 0 {
		t.Fatalf("revision 变化后必须清空答案状态，got %d", len(executor.answerStates))
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "重新") || !strings.Contains(reply, "/con") {
		t.Fatalf("回复 = %q, want 提示重新 /con", reply)
	}
}

func TestExecutorNewSessionSelectsCreatedTarget(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	selectExecutorHostEntry(t, executor)
	api.createResult = CreatedSession{ID: "new-1", Title: "新任务"}

	executor.HandleMessage(context.Background(), executorIncoming("new-session", "/new-session 新任务 /tmp"))

	if api.createCalls != 1 || api.createCwd != "/tmp" || api.createTitle != "新任务" {
		t.Fatalf("Create 调用错误：calls=%d cwd=%q title=%q", api.createCalls, api.createCwd, api.createTitle)
	}
	selected, err := executor.SelectedTarget()
	if err != nil {
		t.Fatalf("新会话创建后 SelectedTarget() 返回错误：%v", err)
	}
	want := executor.sessionTarget(SessionSummary{ID: "new-1", Title: "新任务", Cwd: "/tmp", Available: true})
	if selected.PaneID != want.PaneID || selected.OccupantKey != want.OccupantKey || selected.Title != "新任务" {
		t.Fatalf("selected = %#v, want %#v", selected, want)
	}
	created, ok := executor.TakeCreatedTarget()
	if !ok || created != selected {
		t.Fatalf("TakeCreatedTarget() = (%#v, %v), want (%#v, true)", created, ok, selected)
	}
	if _, ok := executor.TakeCreatedTarget(); ok {
		t.Fatal("TakeCreatedTarget() 第二次调用必须返回 false")
	}
	reply := sink.lastReply()
	if !strings.Contains(reply, "新任务") || !strings.Contains(reply, "/tmp") || !strings.Contains(reply, "已切换到新会话") {
		t.Fatalf("回复 = %q, want 标题、目录与切换提示", reply)
	}

	targets := executor.CurrentTargets()
	if len(targets) != 1 || targets[0] != selected {
		t.Fatalf("CurrentTargets() = %#v, want 仅新会话", targets)
	}

	// session/list 尚未追上时，新会话在宽限期内继续可用。
	executor.refresh(context.Background())
	if targets := executor.CurrentTargets(); len(targets) != 1 || targets[0] != selected {
		t.Fatalf("宽限期内新会话不应消失：%#v", targets)
	}
	if _, err := executor.SelectedTarget(); err != nil {
		t.Fatalf("宽限期内选择不应失效：%v", err)
	}
}

func TestExecutorClearsCreatedTargetOnNextMessage(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	selectExecutorHostEntry(t, executor)
	api.createResult = CreatedSession{ID: "new-1", Title: "新任务"}

	executor.HandleMessage(context.Background(), executorIncoming("new-session", "/new-session 新任务 /tmp"))
	if _, ok := executor.TakeCreatedTarget(); !ok {
		t.Fatal("创建成功后应可领取创建目标")
	}

	executor.HandleMessage(context.Background(), executorIncoming("new-session-2", "/new-session 新任务 /tmp"))
	if _, ok := executor.TakeCreatedTarget(); !ok {
		t.Fatal("本次 /new-session 应设置创建目标")
	}
	// 下一条普通消息必须清除尚未领取的旧创建目标，避免关联到错误命令结果。
	executor.HandleMessage(context.Background(), executorIncoming("key", "/key enter"))
	if _, ok := executor.TakeCreatedTarget(); ok {
		t.Fatal("后续消息开始时应清除旧创建目标")
	}
}

func TestExecutorCreatedSessionGraceExpires(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	selectExecutorHostEntry(t, executor)
	api.createResult = CreatedSession{ID: "new-1", Title: "新任务"}

	executor.HandleMessage(context.Background(), executorIncoming("new-session", "/new-session 新任务 /tmp"))
	selected, err := executor.SelectedTarget()
	if err != nil {
		t.Fatalf("SelectedTarget() 返回错误：%v", err)
	}

	executor.mu.Lock()
	state := executor.sessions[selected.PaneID]
	state.pendingUntil = time.Now().Add(-time.Second)
	executor.sessions[selected.PaneID] = state
	executor.mu.Unlock()

	executor.refresh(context.Background())
	if targets := executor.CurrentTargets(); len(targets) != 1 || targets[0].Tab != hostEntryTab {
		t.Fatalf("宽限期结束后未出现在 session/list 的会话应移除，got %#v", targets)
	}
}

func TestExecutorNewSessionValidatesDirectoryAndDefaults(t *testing.T) {
	t.Run("拒绝不存在的目录", func(t *testing.T) {
		executor, api, sink := newExecutorFixture(t, testValidHost())
		selectExecutorHostEntry(t, executor)
		missing := filepath.Join(t.TempDir(), "missing")

		executor.HandleMessage(context.Background(), executorIncoming("bad-dir", "/new-session 标题 "+missing))

		if api.createCalls != 0 {
			t.Fatalf("目录不存在时不应调用 Create，got %d", api.createCalls)
		}
		if reply := sink.lastReply(); !strings.Contains(reply, "目录") {
			t.Fatalf("回复 = %q, want 目录错误提示", reply)
		}
	})

	t.Run("默认标题与用户主目录", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		executor, api, _ := newExecutorFixture(t, testValidHost())
		selectExecutorHostEntry(t, executor)
		api.createResult = CreatedSession{ID: "new-1", Title: "默认标题"}

		executor.HandleMessage(context.Background(), executorIncoming("defaults", "/new-session"))

		if api.createCalls != 1 || api.createCwd != home || api.createTitle != "默认标题" {
			t.Fatalf("Create 调用错误：calls=%d cwd=%q title=%q", api.createCalls, api.createCwd, api.createTitle)
		}
	})

	t.Run("展开用户主目录", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		work := filepath.Join(home, "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatalf("创建测试目录失败：%v", err)
		}
		executor, api, _ := newExecutorFixture(t, testValidHost())
		selectExecutorHostEntry(t, executor)
		api.createResult = CreatedSession{ID: "new-2", Title: "修复"}

		executor.HandleMessage(context.Background(), executorIncoming("tilde", "/new-session 修复 ~/work"))

		if api.createCalls != 1 || api.createCwd != work {
			t.Fatalf("Create 调用错误：calls=%d cwd=%q, want %q", api.createCalls, api.createCwd, work)
		}
	})
}

func TestExecutorHostEntryOnlySupportsNewSession(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	host := selectExecutorHostEntry(t, executor)

	executor.HandleMessage(context.Background(), executorIncoming("host-prompt", "你好"))

	if len(api.promptCalls) != 0 {
		t.Fatalf("宿主入口不应调用 Prompt，got %#v", api.promptCalls)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "宿主入口") || !strings.Contains(reply, "/new-session") {
		t.Fatalf("回复 = %q, want 宿主入口说明", reply)
	}

	executor.HandleMessage(context.Background(), executorIncoming("host-answer", "/answer 1"))
	if len(api.submitCalls) != 0 {
		t.Fatalf("宿主入口不应提交回答，got %#v", api.submitCalls)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "/new-session") {
		t.Fatalf("回复 = %q, want 仅支持 /new-session", reply)
	}

	content, err := executor.ReadTerminalSnapshot(context.Background(), host.PaneID, host.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil {
		t.Fatalf("宿主入口读取返回错误：%v", err)
	}
	if !strings.Contains(content.Text, "/new-session") {
		t.Fatalf("宿主入口快照 = %q, want 说明", content.Text)
	}
}

func TestExecutorKeyRepliesUnsupported(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("key", "/key enter"))

	reply := sink.lastReply()
	if !strings.Contains(reply, "dsh 不支持终端按键") || !strings.Contains(reply, "/answer") {
		t.Fatalf("回复 = %q, want 不支持按键并提示 /answer", reply)
	}
	if len(api.promptCalls) != 0 || len(api.submitCalls) != 0 {
		t.Fatalf("按键命令不应触发其他 API 调用：%#v %#v", api.promptCalls, api.submitCalls)
	}
}

func TestExecutorContentPaging(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.readText = "1\n2\n3"
	selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("paging-con", "/con"))
	if got := sink.lastReply(); got != "1\n2\n3" {
		t.Fatalf("/con = %q, want 最新三行", got)
	}

	api.readText = "0\n1\n2\n3"
	executor.HandleMessage(context.Background(), executorIncoming("paging-up", "/pageup"))
	if got := sink.lastReply(); got != "0" {
		t.Fatalf("/pageup = %q, want 更早一页", got)
	}

	executor.HandleMessage(context.Background(), executorIncoming("paging-down", "/pagedn"))
	if got := sink.lastReply(); got != "1\n2\n3" {
		t.Fatalf("/pagedn = %q, want 最新一页", got)
	}

	executor.HandleMessage(context.Background(), executorIncoming("paging-down-newest", "/pagedn"))
	if got := sink.lastReply(); !strings.Contains(got, "最新") {
		t.Fatalf("/pagedn 在最新页回复 = %q, want 最新提示", got)
	}
}

func TestExecutorRejectsUnauthorizedAndMissingMessageID(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	selectExecutorSession(t, executor, api, "s1")

	unauthorized := executorIncoming("unauthorized", "不得转发")
	unauthorized.UserID = "other"
	executor.HandleMessage(context.Background(), unauthorized)
	if len(api.promptCalls) != 0 || len(sink.replies) != 0 {
		t.Fatalf("未授权用户不应产生回复或调用：%#v %#v", api.promptCalls, sink.replies)
	}

	executor.HandleMessage(context.Background(), executorIncoming("", "继续"))
	if len(api.promptCalls) != 0 {
		t.Fatalf("缺失 MessageID 时不应调用 Prompt，got %#v", api.promptCalls)
	}
	if reply := sink.lastReply(); !strings.Contains(reply, "消息标识缺失") {
		t.Fatalf("回复 = %q, want 消息标识缺失提示", reply)
	}
}

func TestExecutorTruncatesOversizedLabelsForHPRPSnapshot(t *testing.T) {
	executor, api, _ := newExecutorFixture(t, testValidHost())
	chineseTitle := strings.Repeat("中", 256) // 768 字节，超过 HPRP 的 512 字节标签上限。
	emojiTitle := strings.Repeat("🙂", 256)   // 1024 字节，截断必须落在 UTF-8 边界。
	api.sessions = []SessionSummary{
		{ID: "s1", Title: chineseTitle, Cwd: "/tmp/" + chineseTitle, Available: true},
		{ID: "s2", Title: emojiTitle, Cwd: "/tmp/" + emojiTitle, Available: true},
	}

	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) != 2 {
		t.Fatalf("CurrentTargets() 数量 = %d, want 2", len(targets))
	}
	for _, target := range targets {
		if len(target.Title) > hprp.MaxLabelBytes || !utf8.ValidString(target.Title) {
			t.Fatalf("Title 长度 = %d（%q），必须 <= %d 且 UTF-8 有效", len(target.Title), target.Title, hprp.MaxLabelBytes)
		}
		if len(target.Workspace) > hprp.MaxLabelBytes || !utf8.ValidString(target.Workspace) {
			t.Fatalf("Workspace 长度 = %d（%q），必须 <= %d 且 UTF-8 有效", len(target.Workspace), target.Workspace, hprp.MaxLabelBytes)
		}
	}

	// 用生产快照构建器验证整条快照可通过 Server 校验，而不是只检查单个字段。
	snapshot := relayclient.BuildSnapshot(1, targets)
	if err := hprp.ValidateSessionSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateSessionSnapshot() 返回错误：%v（快照 = %#v）", err, snapshot)
	}
	if got := snapshot.Sessions[0].Display.Title; got != strings.Repeat("中", hprp.MaxLabelBytes/3) {
		t.Fatalf("中文标题截断结果 = %q，want 170 个完整字符", got)
	}
}

func TestExecutorReadShowsCurrentQuestionDuringCollection(t *testing.T) {
	executor, api, sink := newExecutorFixture(t, testValidHost())
	api.sessions = []SessionSummary{{ID: "s1", Title: "任务", Available: true}}
	api.decisions = []Decision{testTwoQuestionForm("s1", "rev-1")}
	api.readText = "用户：开始"
	target := selectExecutorSession(t, executor, api, "s1")

	executor.HandleMessage(context.Background(), executorIncoming("answer-first", "/answer 1"))
	if reply := sink.lastReply(); !strings.Contains(reply, "第二题") {
		t.Fatalf("第一题回答后应推进到第二题：%q", reply)
	}

	executor.HandleMessage(context.Background(), executorIncoming("con-progress", "/con"))
	reply := sink.lastReply()
	if !strings.Contains(reply, "(2/2)") || !strings.Contains(reply, "第二题") || strings.Contains(reply, "第一题") {
		t.Fatalf("/con 应展示当前第 2 题而不是第 1 题：%q", reply)
	}

	content, err := executor.ReadTerminalSnapshot(context.Background(), target.PaneID, target.OccupantKey, im.OutputModeText, panel.PageSize)
	if err != nil {
		t.Fatalf("ReadTerminalSnapshot() 返回错误：%v", err)
	}
	if !strings.Contains(content.Text, "(2/2)") || !strings.Contains(content.Text, "第二题") {
		t.Fatalf("终端快照应展示当前题目：%q", content.Text)
	}
}

func TestExecutorRefreshPendingFailureConcurrentNewSession(t *testing.T) {
	// 回归：Pending 失败时 carryForwardBlocking 曾在锁外读取 e.sessions，
	// 与 /new-session 的写入并发会触发 concurrent map read/write。
	executor, api, _ := newExecutorFixture(t, testValidHost())
	executor.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// 本测试只关注目录 map；移除 sink 避免通知测试替身成为并发噪声来源。
	executor.sink = nil
	api.pendingErr = errors.New("Mux 不可用")
	api.createResult = CreatedSession{ID: "created", Title: "并发任务"}
	for index := 0; index < 128; index++ {
		api.sessions = append(api.sessions, SessionSummary{
			ID: fmt.Sprintf("s%03d", index), Title: fmt.Sprintf("任务 %03d", index), Available: true,
		})
	}
	executor.refresh(context.Background())
	targets := executor.CurrentTargets()
	if len(targets) == 0 {
		t.Fatal("初始目录为空")
	}
	if err := executor.SelectTarget(targets[0].PaneID, targets[0].OccupantKey); err != nil {
		t.Fatalf("SelectTarget() 返回错误：%v", err)
	}

	var stop atomic.Bool
	var refreshers sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		refreshers.Add(1)
		go func() {
			defer refreshers.Done()
			for !stop.Load() {
				executor.refresh(context.Background())
			}
		}()
	}
	for index := 0; index < 200; index++ {
		executor.HandleMessage(context.Background(),
			executorIncoming(fmt.Sprintf("new-session-%d", index), "/new-session 并发任务 /tmp"))
	}
	stop.Store(true)
	refreshers.Wait()
}
