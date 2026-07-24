package bridge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
			if len(calls) != 1 || calls[0] != (notifierReadCall{target: "pane-1", lines: 100}) {
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

func TestNotifierSnapshotValidationUsesPaneIDInOrder(t *testing.T) {
	for _, current := range []herdr.AgentStatus{herdr.AgentStatusBlocked, herdr.AgentStatusDone} {
		t.Run(string(current), func(t *testing.T) {
			trace := &notifierCallTrace{}
			getter := &notifierAgentGetter{agent: notificationAgentInfo(""), trace: trace}
			reader := &notifierReader{
				result: herdr.ReadResult{PaneID: "pane-1", Text: "终端近期快照"},
				trace:  trace,
			}
			notifier := mustNotifierWithGetter(t, &notifierIM{}, getter.GetAgent, reader.ReadRecent)

			if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusWorking, current)); err != nil {
				t.Fatalf("HandleTransition() 返回错误：%v", err)
			}
			if got, want := trace.Calls(), []notifierTargetCall{
				{method: "get", target: "pane-1"},
				{method: "read", target: "pane-1", lines: 100},
				{method: "get", target: "pane-1"},
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("occupant/read 调用顺序 = %#v, want %#v", got, want)
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
	getter := &notifierAgentGetter{agent: notificationAgentInfo("")}
	notifier := mustNotifierWithGetter(t, im, getter.GetAgent, reader.ReadRecent)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked)

	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("首次 HandleTransition() 返回错误：%v", err)
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("重复 HandleTransition() 返回错误：%v", err)
	}
	transition.Target = notificationTargetWithSession("session-2")
	getter.SetAgent(notificationAgentInfo("session-2"))
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

func TestNotifierPreReadOccupantValidationFailureSkipsTerminalRead(t *testing.T) {
	tests := []struct {
		name   string
		agent  herdr.AgentInfo
		getErr error
	}{
		{name: "occupant replaced", agent: notificationAgentInfo("session-new")},
		{name: "get failed", getErr: errors.New("get failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := notificationTargetWithSession("session-old")
			getter := &notifierAgentGetter{agent: test.agent, err: test.getErr}
			reader := &notifierReader{result: herdr.ReadResult{PaneID: target.PaneID, Text: "不应读取"}}
			im := &notifierIM{}
			notifier := mustNotifierWithGetter(t, im, getter.GetAgent, reader.ReadRecent)

			transition := session.Transition{Target: target, Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusBlocked}
			if err := notifier.HandleTransition(context.Background(), transition); err != nil {
				t.Fatalf("HandleTransition() 返回错误：%v", err)
			}
			if calls := reader.Calls(); len(calls) != 0 {
				t.Fatalf("occupant 前置校验失败后仍读取终端：%#v", calls)
			}
			messages := im.Messages()
			if len(messages) != 1 || !strings.Contains(messages[0], "已阻塞") || strings.Contains(messages[0], "不应读取") {
				t.Fatalf("前置校验失败通知 = %#v", messages)
			}
		})
	}
}

func TestNotifierPostReadOccupantReplacementDoesNotLeakSnapshot(t *testing.T) {
	target := notificationTargetWithSession("session-old")
	trace := &notifierCallTrace{}
	getter := &notifierAgentGetter{agent: notificationAgentInfo("session-old"), trace: trace}
	reader := &blockingNotifierReader{
		result:  herdr.ReadResult{PaneID: target.PaneID, Text: "SENSITIVE-MARKER"},
		started: make(chan struct{}),
		release: make(chan struct{}),
		trace:   trace,
	}
	im := &notifierIM{}
	notifier := mustNotifierWithGetter(t, im, getter.GetAgent, reader.ReadRecent)
	transition := session.Transition{Target: target, Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusDone}
	result := make(chan error, 1)
	go func() { result <- notifier.HandleTransition(context.Background(), transition) }()
	<-reader.started

	getter.SetAgent(notificationAgentInfo("session-new"))
	close(reader.release)
	if err := <-result; err != nil {
		t.Fatalf("HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "已完成") || strings.Contains(strings.Join(messages, "\n"), "SENSITIVE-MARKER") {
		t.Fatalf("post-read occupant 替换后泄露终端快照：%#v", messages)
	}
	if got := getter.CallCount(); got != 2 {
		t.Fatalf("GetAgent 调用次数 = %d，期望读取前后各一次", got)
	}
	if got, want := trace.Calls(), []notifierTargetCall{
		{method: "get", target: "pane-1"},
		{method: "read", target: "pane-1", lines: 100},
		{method: "get", target: "pane-1"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("occupant/read 调用顺序 = %#v, want %#v", got, want)
	}
}

func TestNotifierPartialSnapshotRetryRevalidatesOccupant(t *testing.T) {
	target := notificationTargetWithSession("session-old")
	getter := &notifierAgentGetter{agent: notificationAgentInfo("session-old")}
	reader := &notifierReader{result: herdr.ReadResult{PaneID: target.PaneID, Text: "SENSITIVE-PARTIAL-MARKER"}}
	im := &failOnceNotifierIM{failAt: 2}
	notifier := mustNotifierWithGetter(t, im, getter.GetAgent, reader.ReadRecent)
	transition := session.Transition{Target: target, Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusDone}

	if err := notifier.HandleTransition(context.Background(), transition); err == nil {
		t.Fatal("首轮快照分段失败时 HandleTransition() 未返回错误")
	}
	getter.SetAgent(notificationAgentInfo("session-new"))
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("occupant 替换后重试返回错误：%v", err)
	}
	messages := im.Messages()
	joined := strings.Join(messages, "\n")
	if strings.Contains(joined, "SENSITIVE-PARTIAL-MARKER") || strings.Count(joined, "已完成") != 1 || len(messages) != 1 {
		t.Fatalf("分段重试泄露旧 occupant 快照或重复标题：%#v", messages)
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
	im := &failOnceNotifierIM{failAt: 2}
	notifier := mustNotifier(t, im, reader.ReadRecent)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)

	if err := notifier.HandleTransition(context.Background(), transition); err == nil {
		t.Fatal("首轮分段发送失败时 HandleTransition() 未返回错误")
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("重试 HandleTransition() 返回错误：%v", err)
	}
	messages := im.Messages()
	if len(messages) != 2 || im.CallCount() != 3 || strings.Count(strings.Join(messages, "\n"), "已完成") != 1 {
		t.Fatalf("失败后未从失败分段继续：calls=%d messages=%#v", im.CallCount(), messages)
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

func TestNotificationDispatcherCoalescesLatestStatusPerPane(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	for _, transition := range []session.Transition{
		notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking),
		notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked),
		notificationTransition(herdr.AgentStatusBlocked, herdr.AgentStatusDone),
	} {
		if err := dispatcher.EnqueueStatus(epoch, transition); err != nil {
			t.Fatalf("同 pane 状态合并时 EnqueueStatus() 返回错误：%v", err)
		}
	}
	close(im.firstRelease)
	awaitNotifierCondition(t, "合并后的最新状态通知", func() bool { return len(im.Messages()) == 2 })
	messages := strings.Join(im.Messages(), "\n")
	if !strings.Contains(messages, "目标已失效") || !strings.Contains(messages, "已完成") || strings.Contains(messages, "开始工作") || strings.Contains(messages, "已阻塞") {
		t.Fatalf("同 pane 状态未合并为最新任务：%q", messages)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherDropsOldEpochStatusButKeepsInvalidation(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	oldEpoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	if err := dispatcher.EnqueueStatus(oldEpoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("旧 epoch EnqueueStatus() 返回错误：%v", err)
	}
	dispatcher.EndEpoch(oldEpoch)
	newEpoch := dispatcher.BeginEpoch()
	if err := dispatcher.EnqueueStatus(newEpoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusUnknown)); err != nil {
		t.Fatalf("新 epoch EnqueueStatus() 返回错误：%v", err)
	}
	close(im.firstRelease)
	awaitNotifierCondition(t, "跨 epoch 通知", func() bool { return len(im.Messages()) == 2 })
	messages := strings.Join(im.Messages(), "\n")
	if !strings.Contains(messages, "目标已失效") || !strings.Contains(messages, "无法可靠识别") || strings.Contains(messages, "已完成") {
		t.Fatalf("epoch 清理或 invalidation 保留不正确：%q", messages)
	}

	dispatcher.EndEpoch(newEpoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherStaleEpochDoesNotEvictCurrentStatus(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	blocker := notificationTarget()
	blocker.PaneID = "pane-2"
	blocker.OccupantKey = "occupant-pane-2"
	if err := dispatcher.EnqueueInvalidated(blocker); err != nil {
		t.Fatalf("blocker EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("当前 epoch EnqueueStatus() 返回错误：%v", err)
	}
	if err := dispatcher.EnqueueStatus(epoch+1, notificationTransition(herdr.AgentStatusDone, herdr.AgentStatusIdle)); err != nil {
		t.Fatalf("过期 epoch EnqueueStatus() 返回错误：%v", err)
	}
	close(im.firstRelease)
	awaitNotifierCondition(t, "当前 epoch 状态保留", func() bool { return len(im.Messages()) == 2 })
	if messages := strings.Join(im.Messages(), "\n"); !strings.Contains(messages, "目标已失效") || !strings.Contains(messages, "已完成") {
		t.Fatalf("过期 epoch 错误淘汰当前状态：%q", messages)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherRetriesInvalidationWithInjectedWait(t *testing.T) {
	im := &notifierIM{failAt: 1}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	retry := &notificationRetry{delay: 7 * time.Second}
	waiter := newNotificationRetryWaiter()
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 2,
		Backoff:  retry,
		Wait:     waiter.Wait,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	wait := <-waiter.started
	if wait.delay != 7*time.Second || retry.NextCount() != 1 {
		t.Fatalf("通知重试等待 = %v，Next=%d", wait.delay, retry.NextCount())
	}
	im.SetFailAt(0)
	close(wait.release)
	awaitNotifierCondition(t, "invalidation 重试成功", func() bool { return len(im.Messages()) == 2 })
	if retry.ResetCount() != 1 {
		t.Fatalf("通知成功后 Reset 次数 = %d，期望 1", retry.ResetCount())
	}

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherInvalidationCancelsSamePaneStatusRetry(t *testing.T) {
	im := &failOnceNotifierIM{failAt: 1}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	waiter := newNotificationRetryWaiter()
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 2,
		Backoff:  &notificationRetry{delay: time.Minute},
		Wait:     waiter.Wait,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
		t.Fatalf("EnqueueStatus() 返回错误：%v", err)
	}
	wait := <-waiter.started
	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	if err := <-wait.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("旧状态重试等待 = %v，期望被 invalidation 取消", err)
	}
	awaitNotifierCondition(t, "invalidation 越过旧状态重试", func() bool { return len(im.Messages()) == 1 })
	if message := im.Messages()[0]; !strings.Contains(message, "目标已失效") || strings.Contains(message, "开始工作") {
		t.Fatalf("invalidation 竞争结果 = %q", message)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherRetriesOnlyFailedNotificationParts(t *testing.T) {
	im := &failOnceNotifierIM{failAt: 2}
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: "快照内容"}}
	notifier := mustNotifier(t, im, reader.ReadRecent)
	waiter := newNotificationRetryWaiter()
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waiter.Wait,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("EnqueueStatus() 返回错误：%v", err)
	}
	wait := <-waiter.started
	close(wait.release)
	awaitNotifierCondition(t, "剩余通知分段重试", func() bool { return len(im.Messages()) == 2 })
	messages := im.Messages()
	if im.CallCount() != 3 || strings.Count(strings.Join(messages, "\n"), "已完成") != 1 || !strings.Contains(messages[1], "快照内容") {
		t.Fatalf("分段重试重复已成功内容：calls=%d messages=%#v", im.CallCount(), messages)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherKeepsInvalidationProgressAcrossReset(t *testing.T) {
	target := notificationTarget()
	target.Title = strings.Repeat("中", panel.WeComContentLimit)
	expected := renderStatusTitleParts("Agent 目标已失效，请重新执行 /ls 和 /sel。", target)
	if len(expected) < 2 {
		t.Fatalf("测试目标未生成多段 invalidation：%d", len(expected))
	}
	im := &failOnceNotifierIM{failAt: 2}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	waiter := newNotificationRetryWaiter()
	retry := &notificationRetry{delay: time.Second}
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  retry,
		Wait:     waiter.Wait,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	if err := dispatcher.EnqueueInvalidated(target); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	wait := <-waiter.started
	notifier.Reset()
	close(wait.release)
	awaitNotifierCondition(t, "跨 Reset invalidation 重试完成", func() bool { return retry.ResetCount() == 1 })
	if messages := im.Messages(); !reflect.DeepEqual(messages, expected) || im.CallCount() != len(expected)+1 {
		t.Fatalf("跨 Reset 重复已成功 invalidation 分段：calls=%d messages=%#v want=%#v", im.CallCount(), messages, expected)
	}

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherReturnsInjectedWaitError(t *testing.T) {
	waitErr := errors.New("notification wait failed")
	im := &failOnceNotifierIM{failAt: 1}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	retry := &notificationRetry{delay: time.Second}
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  retry,
		Wait: func(context.Context, time.Duration) error {
			return waitErr
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	if err := <-result; !errors.Is(err, waitErr) {
		t.Fatalf("Run() error = %v，期望注入 wait 错误", err)
	}
	if retry.NextCount() != 1 || im.CallCount() != 1 {
		t.Fatalf("wait 错误后发生忙循环：next=%d sends=%d", retry.NextCount(), im.CallCount())
	}
}

func TestNotificationDispatcherCancellationStopsRetryWait(t *testing.T) {
	im := &notifierIM{failAt: 1}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	waiter := newNotificationRetryWaiter()
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Minute},
		Wait:     waiter.Wait,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	wait := <-waiter.started
	cancel()
	if err := <-wait.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("重试等待取消结果 = %v，期望 context.Canceled", err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
	if got := len(im.Messages()); got != 1 {
		t.Fatalf("取消后仍执行重试：messages=%d", got)
	}
}

func TestNotificationDispatcherDeduplicatesPendingInvalidationByOccupant(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	first := notificationTarget()
	first.OccupantKey = "occupant-1"
	if err := dispatcher.EnqueueInvalidated(first); err != nil {
		t.Fatalf("首次 EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	if err := dispatcher.EnqueueInvalidated(first); err != nil {
		t.Fatalf("重复 occupant 不应占用新 slot：%v", err)
	}
	second := first
	second.OccupantKey = "occupant-2"
	if err := dispatcher.EnqueueInvalidated(second); err != nil {
		t.Fatalf("第二个 occupant 应占用唯一待发 slot：%v", err)
	}
	third := first
	third.OccupantKey = "occupant-3"
	if err := dispatcher.EnqueueInvalidated(third); !errors.Is(err, ErrNotificationQueueFull) {
		t.Fatalf("第三个 occupant error = %v，期望 ErrNotificationQueueFull", err)
	}

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherCancellationDoesNotStartPendingTasks(t *testing.T) {
	im := newCancelCountingNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 2,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()

	first := notificationTarget()
	first.OccupantKey = "occupant-1"
	if err := dispatcher.EnqueueInvalidated(first); err != nil {
		t.Fatalf("首次 EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	second := first
	second.OccupantKey = "occupant-2"
	if err := dispatcher.EnqueueInvalidated(second); err != nil {
		t.Fatalf("待发 EnqueueInvalidated() 返回错误：%v", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
	if got := im.CallCount(); got != 1 {
		t.Fatalf("context 取消后仍启动待发通知：calls=%d", got)
	}
}

func TestNotificationDispatcherInvalidationRemovesPendingSamePaneStatus(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	blocker := notificationTarget()
	blocker.PaneID = "pane-2"
	blocker.OccupantKey = "occupant-pane-2"
	if err := dispatcher.EnqueueInvalidated(blocker); err != nil {
		t.Fatalf("blocker EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
		t.Fatalf("EnqueueStatus() 返回错误：%v", err)
	}
	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("invalidation 未复用被淘汰 status 的 slot：%v", err)
	}
	close(im.firstRelease)
	awaitNotifierCondition(t, "pending status 淘汰", func() bool { return len(im.Messages()) == 2 })
	if messages := strings.Join(im.Messages(), "\n"); strings.Contains(messages, "开始工作") || strings.Count(messages, "目标已失效") != 2 {
		t.Fatalf("pending status 未被 invalidation 淘汰：%q", messages)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherNonNotifyTransitionRemovesPendingStatus(t *testing.T) {
	im := newGatedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  &notificationRetry{delay: time.Second},
		Wait:     waitSupervisorDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	blocker := notificationTarget()
	blocker.PaneID = "pane-2"
	blocker.OccupantKey = "occupant-pane-2"
	if err := dispatcher.EnqueueInvalidated(blocker); err != nil {
		t.Fatalf("blocker EnqueueInvalidated() 返回错误：%v", err)
	}
	<-im.firstStarted
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("done EnqueueStatus() 返回错误：%v", err)
	}
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusDone, herdr.AgentStatusIdle)); err != nil {
		t.Fatalf("non-notify idle EnqueueStatus() 返回错误：%v", err)
	}
	close(im.firstRelease)
	awaitNotifierCondition(t, "non-notify 淘汰 pending status", func() bool { return len(im.Messages()) == 1 })
	if messages := strings.Join(im.Messages(), "\n"); strings.Contains(messages, "已完成") || !strings.Contains(messages, "目标已失效") {
		t.Fatalf("non-notify 未淘汰旧 done：%q", messages)
	}

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherLatestTransitionCancelsActiveStatus(t *testing.T) {
	tests := []struct {
		name      string
		first     session.Transition
		next      session.Transition
		wantText  string
		wantCount int
	}{
		{
			name:  "non-notify idle drops done",
			first: notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone),
			next:  notificationTransition(herdr.AgentStatusDone, herdr.AgentStatusIdle),
		},
		{
			name:      "unknown replaces working",
			first:     notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking),
			next:      notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusUnknown),
			wantText:  "无法可靠识别",
			wantCount: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			im := &failOnceNotifierIM{failAt: 1}
			notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
			waiter := newNotificationRetryWaiter()
			dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
				Capacity: 1,
				Backoff:  &notificationRetry{delay: time.Minute},
				Wait:     waiter.Wait,
			})
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- dispatcher.Run(ctx) }()
			epoch := dispatcher.BeginEpoch()

			if err := dispatcher.EnqueueStatus(epoch, test.first); err != nil {
				t.Fatalf("first EnqueueStatus() 返回错误：%v", err)
			}
			wait := <-waiter.started
			if err := dispatcher.EnqueueStatus(epoch, test.next); err != nil {
				t.Fatalf("next EnqueueStatus() 返回错误：%v", err)
			}
			if err := <-wait.done; !errors.Is(err, context.Canceled) {
				t.Fatalf("旧 status 重试等待 = %v，期望被最新迁移取消", err)
			}
			awaitNotifierCondition(t, "最新迁移处理", func() bool { return len(im.Messages()) == test.wantCount })
			if messages := strings.Join(im.Messages(), "\n"); strings.Contains(messages, "已完成") || strings.Contains(messages, "开始工作") || (test.wantText != "" && !strings.Contains(messages, test.wantText)) {
				t.Fatalf("最新迁移未正确取代旧状态：%q", messages)
			}

			dispatcher.EndEpoch(epoch)
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v，期望 context.Canceled", err)
			}
		})
	}
}

func TestNotificationDispatcherSupersedeDiscardsStatusProgress(t *testing.T) {
	tests := []struct {
		name         string
		secondResult herdr.ReadResult
		secondError  error
		wantMessages int
		wantSnapshot string
	}{
		{
			name:         "new done sends title and snapshot",
			secondResult: herdr.ReadResult{PaneID: "pane-1", Text: "新快照"},
			wantMessages: 3,
			wantSnapshot: "新快照",
		},
		{
			name:         "new done without snapshot still sends title",
			secondError:  errors.New("read failed"),
			wantMessages: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &notifierReader{
				results: []herdr.ReadResult{
					{PaneID: "pane-1", Text: "旧快照"},
					test.secondResult,
				},
				errors: []error{nil, test.secondError},
			}
			im := &failOnceNotifierIM{failAt: 2}
			notifier := mustNotifier(t, im, reader.ReadRecent)
			waiter := newCancellationGateWaiter()
			dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
				Capacity: 1,
				Backoff:  &notificationRetry{delay: time.Minute},
				Wait:     waiter.Wait,
			})
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- dispatcher.Run(ctx) }()
			epoch := dispatcher.BeginEpoch()

			if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
				t.Fatalf("首次 done EnqueueStatus() 返回错误：%v", err)
			}
			wait := <-waiter.started
			if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusDone, herdr.AgentStatusIdle)); err != nil {
				t.Fatalf("idle EnqueueStatus() 返回错误：%v", err)
			}
			<-wait.canceled
			if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
				t.Fatalf("working EnqueueStatus() 返回错误：%v", err)
			}
			if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
				t.Fatalf("再次 done EnqueueStatus() 返回错误：%v", err)
			}
			close(wait.release)
			if err := <-wait.done; !errors.Is(err, context.Canceled) {
				t.Fatalf("旧 done 重试等待 = %v，期望 context.Canceled", err)
			}
			awaitNotifierCondition(t, "再次 done 完整发送", func() bool { return len(im.Messages()) == test.wantMessages })
			messages := strings.Join(im.Messages(), "\n")
			if strings.Count(messages, "已完成") != 2 || strings.Contains(messages, "开始工作") {
				t.Fatalf("再次 done 复用了旧分段进度：%#v", im.Messages())
			}
			if test.wantSnapshot != "" && !strings.Contains(messages, test.wantSnapshot) {
				t.Fatalf("再次 done 缺少新快照：%#v", im.Messages())
			}

			dispatcher.EndEpoch(epoch)
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v，期望 context.Canceled", err)
			}
		})
	}
}

func TestNotifierDiscardStatusPreservesInvalidationProgress(t *testing.T) {
	target := notificationTarget()
	target.Title = strings.Repeat("中", panel.WeComContentLimit)
	expected := renderStatusTitleParts("Agent 目标已失效，请重新执行 /ls 和 /sel。", target)
	if len(expected) < 2 {
		t.Fatalf("测试目标未生成多段 invalidation：%d", len(expected))
	}
	im := &failOnceNotifierIM{failAt: 2}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	if err := notifier.TargetInvalidated(context.Background(), target); err == nil {
		t.Fatal("首轮 invalidation 分段发送未失败")
	}

	notifier.discardStatus(target.PaneID)
	if err := notifier.TargetInvalidated(context.Background(), target); err != nil {
		t.Fatalf("invalidation 重试返回错误：%v", err)
	}
	if messages := im.Messages(); !reflect.DeepEqual(messages, expected) {
		t.Fatalf("discardStatus 错误清除 invalidation 进度：%#v", messages)
	}
}

func mustNotifier(t *testing.T, im IMAdapter, read ReadRecentFunc) *Notifier {
	t.Helper()
	return mustNotifierWithGetter(t, im, matchingNotificationAgent, read)
}

func mustNotifierWithGetter(t *testing.T, im IMAdapter, get GetAgentFunc, read ReadRecentFunc) *Notifier {
	t.Helper()
	notifier, err := NewNotifier(im, get, read)
	if err != nil {
		t.Fatalf("NewNotifier() 返回错误：%v", err)
	}
	return notifier
}

func notificationTransition(previous, current herdr.AgentStatus) session.Transition {
	return session.Transition{Target: notificationTarget(), Previous: previous, Current: current}
}

func notificationTarget() session.Target {
	return notificationTargetWithSession("")
}

func notificationTargetWithSession(value string) session.Target {
	var agentSession *herdr.AgentSession
	if value != "" {
		agentSession = &herdr.AgentSession{Source: "claude", Agent: "claude", Kind: "id", Value: value}
	}
	registry := &session.Registry{}
	registry.Replace(herdr.Snapshot{
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "workspace-1"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "tab-1"}},
		Panes: []herdr.Pane{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), Title: stringRef("修复 <问题>"),
			AgentStatus: herdr.AgentStatusWorking, AgentSession: agentSession,
		}},
	}, false)
	return registry.CreateListSnapshot()[0]
}

func matchingNotificationAgent(context.Context, string) (herdr.AgentInfo, error) {
	return notificationAgentInfo(""), nil
}

func notificationAgentInfo(sessionValue string) herdr.AgentInfo {
	var agentSession *herdr.AgentSession
	if sessionValue != "" {
		agentSession = &herdr.AgentSession{Source: "claude", Agent: "claude", Kind: "id", Value: sessionValue}
	}
	return herdr.AgentInfo{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), AgentSession: agentSession,
	}
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
	trace   *notifierCallTrace
}

type notifierAgentGetter struct {
	mu    sync.Mutex
	agent herdr.AgentInfo
	err   error
	calls int
	trace *notifierCallTrace
}

func (g *notifierAgentGetter) GetAgent(_ context.Context, target string) (herdr.AgentInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.trace != nil {
		g.trace.record(notifierTargetCall{method: "get", target: target})
	}
	return g.agent, g.err
}

func (g *notifierAgentGetter) SetAgent(agent herdr.AgentInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.agent = agent
	g.err = nil
}

func (g *notifierAgentGetter) CallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type blockingNotifierReader struct {
	result  herdr.ReadResult
	started chan struct{}
	release chan struct{}
	once    sync.Once
	trace   *notifierCallTrace
}

func (r *blockingNotifierReader) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	if r.trace != nil {
		r.trace.record(notifierTargetCall{method: "read", target: target, lines: lines})
	}
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.result, nil
}

type notifierTargetCall struct {
	method string
	target string
	lines  int
}

type notifierCallTrace struct {
	mu    sync.Mutex
	calls []notifierTargetCall
}

func (t *notifierCallTrace) record(call notifierTargetCall) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, call)
}

func (t *notifierCallTrace) Calls() []notifierTargetCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]notifierTargetCall(nil), t.calls...)
}

func (r *notifierReader) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, notifierReadCall{target: target, lines: lines})
	if r.trace != nil {
		r.trace.record(notifierTargetCall{method: "read", target: target, lines: lines})
	}
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

type failOnceNotifierIM struct {
	mu       sync.Mutex
	failAt   int
	failed   bool
	calls    int
	messages []string
}

type cancelCountingNotifierIM struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
}

func newCancelCountingNotifierIM() *cancelCountingNotifierIM {
	return &cancelCountingNotifierIM{firstStarted: make(chan struct{})}
}

func (i *cancelCountingNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *cancelCountingNotifierIM) SendMarkdown(ctx context.Context, _ string) error {
	i.mu.Lock()
	i.calls++
	call := i.calls
	i.mu.Unlock()
	if call == 1 {
		close(i.firstStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (i *cancelCountingNotifierIM) CallCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

func (i *failOnceNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *failOnceNotifierIM) SendMarkdown(_ context.Context, content string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if !i.failed && i.calls == i.failAt {
		i.failed = true
		return errors.New("send failed")
	}
	i.messages = append(i.messages, content)
	return nil
}

func (i *failOnceNotifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
}

func (i *failOnceNotifierIM) CallCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

type blockingNotifierIM struct {
	mu      sync.Mutex
	count   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type gatedNotifierIM struct {
	mu           sync.Mutex
	messages     []string
	firstStarted chan struct{}
	firstRelease chan struct{}
	once         sync.Once
}

func newGatedNotifierIM() *gatedNotifierIM {
	return &gatedNotifierIM{firstStarted: make(chan struct{}), firstRelease: make(chan struct{})}
}

func (i *gatedNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *gatedNotifierIM) SendMarkdown(ctx context.Context, content string) error {
	blocked := false
	i.once.Do(func() {
		blocked = true
		close(i.firstStarted)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.firstRelease:
		}
	}
	i.mu.Lock()
	i.messages = append(i.messages, content)
	i.mu.Unlock()
	return nil
}

func (i *gatedNotifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
}

type notificationRetry struct {
	mu     sync.Mutex
	delay  time.Duration
	next   int
	resets int
}

func (r *notificationRetry) Next() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return r.delay
}

func (r *notificationRetry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resets++
}

func (r *notificationRetry) NextCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

func (r *notificationRetry) ResetCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resets
}

type notificationRetryWait struct {
	delay   time.Duration
	release chan struct{}
	done    chan error
}

type notificationRetryWaiter struct {
	started chan *notificationRetryWait
}

func newNotificationRetryWaiter() *notificationRetryWaiter {
	return &notificationRetryWaiter{started: make(chan *notificationRetryWait, 4)}
}

func (w *notificationRetryWaiter) Wait(ctx context.Context, delay time.Duration) error {
	request := &notificationRetryWait{delay: delay, release: make(chan struct{}), done: make(chan error, 1)}
	w.started <- request
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-request.release:
	}
	request.done <- err
	return err
}

type cancellationGateWait struct {
	canceled chan struct{}
	release  chan struct{}
	done     chan error
}

type cancellationGateWaiter struct {
	started chan *cancellationGateWait
}

func newCancellationGateWaiter() *cancellationGateWaiter {
	return &cancellationGateWaiter{started: make(chan *cancellationGateWait, 1)}
}

func (w *cancellationGateWaiter) Wait(ctx context.Context, _ time.Duration) error {
	request := &cancellationGateWait{
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		done:     make(chan error, 1),
	}
	w.started <- request
	<-ctx.Done()
	close(request.canceled)
	<-request.release
	err := ctx.Err()
	request.done <- err
	return err
}

func awaitNotifierCondition(t *testing.T, name string, condition func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatalf("等待%s超时", name)
		default:
			time.Sleep(time.Millisecond)
		}
	}
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
