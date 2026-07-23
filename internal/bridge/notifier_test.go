package bridge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestNotifierWorkingSendsShortMessageWithoutReading(t *testing.T) {
	reader := &notifierReader{}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)

	err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking))
	if err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	if calls := reader.Calls(); len(calls) != 0 {
		t.Fatalf("working 读取终端：%#v", calls)
	}
	messages := im.Messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "开始工作") || strings.Contains(messages[0], "终端近期快照") {
		t.Fatalf("working 通知 = %#v", messages)
	}
}

func TestNotifierSnapshotPoliciesReadOnlyRecentHundredLines(t *testing.T) {
	tests := []struct {
		name     string
		previous herdr.AgentStatus
		current  herdr.AgentStatus
		wantText string
	}{
		{name: "blocked", previous: herdr.AgentStatusWorking, current: herdr.AgentStatusBlocked, wantText: "已阻塞"},
		{name: "done", previous: herdr.AgentStatusWorking, current: herdr.AgentStatusDone, wantText: "已完成"},
		{name: "working to idle", previous: herdr.AgentStatusWorking, current: herdr.AgentStatusIdle, wantText: "已空闲"},
		{name: "blocked to idle", previous: herdr.AgentStatusBlocked, current: herdr.AgentStatusIdle, wantText: "已空闲"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: numberedNotificationLines(130)}}
			im := &notifierIM{}
			notifier := mustNotifier(t, im, reader.ReadRecent)

			if err := notifier.HandleTransition(context.Background(), notificationTransition(test.previous, test.current)); err != nil {
				t.Fatalf("HandleTransition() 返回错误：%v", err)
			}
			calls := reader.Calls()
			if len(calls) != 1 || calls[0] != (notifierReadCall{target: "terminal-1", lines: 100}) {
				t.Fatalf("ReadRecent 调用 = %#v", calls)
			}
			messages := im.Messages()
			if len(messages) < 2 {
				t.Fatalf("通知条数 = %d，内容：%#v", len(messages), messages)
			}
			if !strings.Contains(messages[0], test.wantText) {
				t.Fatalf("状态标题 = %q，期望包含 %q", messages[0], test.wantText)
			}
			if !strings.HasPrefix(messages[1], "终端近期快照\n") || !strings.Contains(messages[1], "范围：最近最多 100 行") {
				t.Fatalf("快照标题不符合约定：%q", messages[1])
			}
			snapshot := strings.Join(messages[1:], "\n")
			if strings.Contains(snapshot, "line-029") || !strings.Contains(snapshot, "line-030") || !strings.Contains(snapshot, "line-129") {
				t.Fatalf("快照未限制为规范化后的最后 100 行：%q", snapshot)
			}
		})
	}
}

func TestNotifierIgnoresIdleWithoutWorkingOrBlockedTransition(t *testing.T) {
	reader := &notifierReader{}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)

	for _, previous := range []herdr.AgentStatus{herdr.AgentStatusDone, herdr.AgentStatusIdle, herdr.AgentStatusUnknown} {
		if err := notifier.HandleTransition(context.Background(), notificationTransition(previous, herdr.AgentStatusIdle)); err != nil {
			t.Fatalf("HandleTransition(%s -> idle) 返回错误：%v", previous, err)
		}
	}
	if len(reader.Calls()) != 0 || len(im.Messages()) != 0 {
		t.Fatalf("无需通知的 idle 产生副作用：reads=%#v messages=%#v", reader.Calls(), im.Messages())
	}
}

func TestNotifierUnknownWarnsWithoutReadingOrClaimingCompletion(t *testing.T) {
	reader := &notifierReader{}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)

	if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusUnknown)); err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(reader.Calls()) != 0 || len(messages) != 1 || !strings.Contains(messages[0], "无法可靠识别") || strings.Contains(messages[0], "完成") {
		t.Fatalf("unknown 通知不正确：reads=%#v messages=%#v", reader.Calls(), messages)
	}
}

func TestNotifierSuppressesSameNormalizedSnapshotAndAllowsDistinctKey(t *testing.T) {
	reader := &notifierReader{results: []herdr.ReadResult{
		{PaneID: "pane-1", Text: "\x1b[31m相同内容\x1b[0m\n"},
		{PaneID: "pane-1", Text: "相同内容\n"},
		{PaneID: "pane-1", Text: "相同内容\n"},
	}}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked)

	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("首次 HandleTransition() 返回错误：%v", err)
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("重复 HandleTransition() 返回错误：%v", err)
	}
	transition.Target.OccupantKey = "occupant-2"
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("新 occupant HandleTransition() 返回错误：%v", err)
	}
	if got := len(im.Messages()); got != 4 {
		t.Fatalf("通知条数 = %d，期望首次两条、重复零条、新 occupant 两条", got)
	}
	if got := len(reader.Calls()); got != 3 {
		t.Fatalf("读取次数 = %d，期望为生成快照 hash 读取三次", got)
	}
}

func TestNotifierReadFailureStillSendsStatusTitleAndCanRetryWithSnapshot(t *testing.T) {
	reader := &notifierReader{
		results: []herdr.ReadResult{{}, {PaneID: "pane-1", Text: "恢复后的内容"}},
		errors:  []error{errors.New("read failed"), nil},
	}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked)

	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("读取失败时 HandleTransition() 返回错误：%v", err)
	}
	if messages := im.Messages(); len(messages) != 1 || !strings.Contains(messages[0], "已阻塞") || strings.Contains(messages[0], "read failed") {
		t.Fatalf("读取失败通知 = %#v", messages)
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("恢复后 HandleTransition() 返回错误：%v", err)
	}
	if messages := im.Messages(); len(messages) != 3 || !strings.Contains(messages[2], "恢复后的内容") {
		t.Fatalf("恢复后未补发快照：%#v", messages)
	}
}

func TestNotifierDoesNotModifyManualPanelBuffer(t *testing.T) {
	buffer := &panel.Buffer{}
	buffer.Refresh("occupant-1", []string{"manual-1", "manual-2"})
	before := buffer.Render()
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: "automatic"}}
	notifier := mustNotifier(t, &notifierIM{}, reader.ReadRecent)

	if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked)); err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	if after := buffer.Render(); !reflect.DeepEqual(after, before) {
		t.Fatalf("自动通知修改手工 PanelBuffer：before=%#v after=%#v", before, after)
	}
}

func TestNotifierRejectsMismatchedReadResultWithoutForwardingTerminalContent(t *testing.T) {
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "other-pane", Text: "不应发送的内容"}}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)

	if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(messages) != 1 || strings.Contains(messages[0], "不应发送的内容") {
		t.Fatalf("pane 不匹配时泄露终端内容：%#v", messages)
	}
}

func TestNotifierDeliveryFailureDoesNotCommitDedupeState(t *testing.T) {
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: "内容"}}
	im := &notifierIM{failAt: 2}
	notifier := mustNotifier(t, im, reader.ReadRecent)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)

	if err := notifier.HandleTransition(context.Background(), transition); err == nil {
		t.Fatal("首轮分段发送失败时 HandleTransition() 未返回错误")
	}
	im.SetFailAt(0)
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("重试 HandleTransition() 返回错误：%v", err)
	}
	if messages := im.Messages(); len(messages) != 4 {
		t.Fatalf("失败后同一事件未完整重试：%#v", messages)
	}
}

func TestNotifierSnapshotSplittingIsUTF8AndCodeFenceSafe(t *testing.T) {
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: strings.Repeat("中", panel.WeComContentLimit)}}
	im := &notifierIM{}
	notifier := mustNotifier(t, im, reader.ReadRecent)

	if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked)); err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(messages) < 3 {
		t.Fatalf("长快照未分段：共 %d 条", len(messages))
	}
	for index, message := range messages[1:] {
		if len(message) > panel.WeComContentLimit || !strings.HasSuffix(message, "\n```") || strings.Count(message, "```") != 2 {
			t.Fatalf("快照分段 %d 不安全：bytes=%d content=%q", index, len(message), message)
		}
	}
}

func TestNotifierStatusTitleIsSplitWithinMarkdownLimit(t *testing.T) {
	im := &notifierIM{}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	transition := notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)
	transition.Target.Title = strings.Repeat("中", panel.WeComContentLimit)

	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(messages) < 2 {
		t.Fatalf("超长状态标题未分段：%#v", messages)
	}
	for index, message := range messages {
		if len(message) > panel.WeComContentLimit || !utf8.ValidString(message) {
			t.Fatalf("状态标题分段 %d 不安全：bytes=%d", index, len(message))
		}
	}
	if !strings.Contains(strings.Join(messages, ""), "中") {
		t.Fatal("状态标题分段丢失 UTF-8 内容")
	}
}

func TestNotifierTargetInvalidatedIsDeduplicatedByOccupant(t *testing.T) {
	im := &notifierIM{}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	target := notificationTarget()

	if err := notifier.TargetInvalidated(context.Background(), target); err != nil {
		t.Fatalf("TargetInvalidated() 返回错误：%v", err)
	}
	if err := notifier.TargetInvalidated(context.Background(), target); err != nil {
		t.Fatalf("重复 TargetInvalidated() 返回错误：%v", err)
	}
	target.OccupantKey = "occupant-2"
	if err := notifier.TargetInvalidated(context.Background(), target); err != nil {
		t.Fatalf("新 occupant TargetInvalidated() 返回错误：%v", err)
	}
	if messages := im.Messages(); len(messages) != 2 || !strings.Contains(messages[0], "目标已失效") {
		t.Fatalf("目标失效通知 = %#v", messages)
	}
}

func TestNotifierResetDoesNotLetOldInflightDeliveryRestoreDedupe(t *testing.T) {
	im := &blockingNotifierIM{started: make(chan struct{}), release: make(chan struct{})}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	transition := notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)
	result := make(chan error, 1)
	go func() { result <- notifier.HandleTransition(context.Background(), transition) }()
	<-im.started

	notifier.Reset()
	close(im.release)
	if err := <-result; err != nil {
		t.Fatalf("旧周期 HandleTransition() 返回错误：%v", err)
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("新周期 HandleTransition() 返回错误：%v", err)
	}
	if got := im.Count(); got != 2 {
		t.Fatalf("Reset 后同一状态通知次数 = %d，期望新周期重新发送", got)
	}
}

func mustNotifier(t *testing.T, im IMAdapter, read ReadRecentFunc) *Notifier {
	t.Helper()
	notifier, err := NewNotifier(im, read)
	if err != nil {
		t.Fatalf("NewNotifier() 返回错误：%v", err)
	}
	return notifier
}

func notificationTransition(previous, current herdr.AgentStatus) session.Transition {
	return session.Transition{Target: notificationTarget(), Previous: previous, Current: current}
}

func notificationTarget() session.Target {
	return session.Target{PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occupant-1", Agent: "claude", DisplayAgent: "Claude", Title: "修复 <问题>", Status: herdr.AgentStatusWorking}
}

func numberedNotificationLines(count int) string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index)
	}
	return strings.Join(lines, "\n")
}

type notifierReadCall struct {
	target string
	lines  int
}

type notifierReader struct {
	mu      sync.Mutex
	result  herdr.ReadResult
	err     error
	results []herdr.ReadResult
	errors  []error
	calls   []notifierReadCall
}

func (r *notifierReader) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, notifierReadCall{target: target, lines: lines})
	index := len(r.calls) - 1
	result, err := r.result, r.err
	if index < len(r.results) {
		result = r.results[index]
	}
	if index < len(r.errors) {
		err = r.errors[index]
	}
	return result, err
}

func (r *notifierReader) Calls() []notifierReadCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notifierReadCall(nil), r.calls...)
}

type notifierIM struct {
	mu       sync.Mutex
	messages []string
	failAt   int
	calls    int
}

type blockingNotifierIM struct {
	mu      sync.Mutex
	count   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (i *blockingNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *blockingNotifierIM) SendMarkdown(context.Context, string) error {
	i.mu.Lock()
	i.count++
	count := i.count
	i.mu.Unlock()
	if count == 1 {
		i.once.Do(func() { close(i.started) })
		<-i.release
	}
	return nil
}

func (i *blockingNotifierIM) Count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.count
}

func (i *notifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *notifierIM) SendMarkdown(_ context.Context, content string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	i.messages = append(i.messages, content)
	if i.failAt > 0 && i.calls == i.failAt {
		return errors.New("send failed")
	}
	return nil
}

func (i *notifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
}

func (i *notifierIM) SetFailAt(call int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failAt = call
}
