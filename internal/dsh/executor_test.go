package dsh

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// errExecutorUnused 表示测试替身未预期被调用的能力。
var errExecutorUnused = errors.New("测试未使用该 dsh 能力")

// fakeExecutorAPI 是单宿主执行器测试替身；只提供目录和待答，其余能力返回固定错误。
type fakeExecutorAPI struct {
	sessions    []SessionSummary
	pending     PendingSnapshot
	sessionsErr error
	pendingErr  error
}

func (f *fakeExecutorAPI) Sessions(context.Context) ([]SessionSummary, error) {
	return append([]SessionSummary(nil), f.sessions...), f.sessionsErr
}

func (f *fakeExecutorAPI) Pending(context.Context) (PendingSnapshot, error) {
	return f.pending, f.pendingErr
}

func (f *fakeExecutorAPI) Read(context.Context, string) (string, error) {
	return "", errExecutorUnused
}

func (f *fakeExecutorAPI) Prompt(context.Context, string, string, string) error {
	return errExecutorUnused
}

func (f *fakeExecutorAPI) Create(context.Context, string, string) (CreatedSession, error) {
	return CreatedSession{}, errExecutorUnused
}

func (f *fakeExecutorAPI) Decisions(context.Context, string) ([]Decision, error) {
	return nil, errExecutorUnused
}

func (f *fakeExecutorAPI) SubmitAnswer(context.Context, Decision, any) (AnswerReceipt, error) {
	return AnswerReceipt{}, errExecutorUnused
}

// fakeExecutorSink 记录执行器发出的通知和命令回复。
type fakeExecutorSink struct {
	targets []im.NotificationTarget
	events  []im.NotificationEvent
}

func (s *fakeExecutorSink) RespondMarkdown(context.Context, string, string) error {
	return nil
}

func (s *fakeExecutorSink) SendNotification(_ context.Context, target im.NotificationTarget, event im.NotificationEvent) error {
	s.targets = append(s.targets, target)
	s.events = append(s.events, event)
	return nil
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
