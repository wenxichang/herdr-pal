package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
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

func TestNotifierSendsStructuredStableTarget(t *testing.T) {
	reader := &notifierReader{}
	sink := &notifierIM{}
	notifier := mustNotifier(t, sink, reader.ReadRecent)

	if err := notifier.HandleTransition(context.Background(), notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
		t.Fatalf("HandleTransition() error = %v", err)
	}
	targets := sink.Targets()
	if len(targets) != 1 {
		t.Fatalf("notification targets = %#v", targets)
	}
	target := targets[0]
	if target.PaneID != "pane-1" || target.OccupantHash == "" || target.Agent != "claude" || target.DisplayAgent != "Claude" || target.Title != "修复 <问题>" {
		t.Fatalf("notification target = %#v", target)
	}
	events := sink.Events()
	if len(events) != 1 || events[0].Kind != im.NotificationKindAgentStatusChanged || events[0].PreviousStatus != "idle" || events[0].Status != "working" || events[0].OccurredAt.IsZero() {
		t.Fatalf("notification events = %#v", events)
	}
}

func TestNotifierAllStatusEventsDoNotReadTerminal(t *testing.T) {
	reader := &notifierReader{result: herdr.ReadResult{PaneID: "pane-1", Text: "不应读取"}}
	sink := &notifierIM{}
	notifier := mustNotifier(t, sink, reader.ReadRecent)

	for _, transition := range []session.Transition{
		notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusBlocked),
		notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone),
		notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusIdle),
		notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusUnknown),
	} {
		if err := notifier.HandleTransition(context.Background(), transition); err != nil {
			t.Fatalf("HandleTransition(%s) error = %v", transition.Current, err)
		}
	}
	if calls := reader.Calls(); len(calls) != 0 {
		t.Fatalf("状态通知读取了终端：%#v", calls)
	}
	if events := sink.Events(); len(events) != 4 {
		t.Fatalf("notification events = %#v", events)
	}
}

func TestNotifierRejectsStaleOccupantWithoutSending(t *testing.T) {
	getter := &notifierAgentGetter{agent: notificationAgentInfo("session-new")}
	sink := &notifierIM{}
	notifier := mustNotifierWithGetter(t, sink, getter.GetAgent, nil)
	transition := session.Transition{
		Target: notificationTargetWithSession("session-old"), Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusDone,
	}

	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("HandleTransition() error = %v", err)
	}
	if events := sink.Events(); len(events) != 0 {
		t.Fatalf("旧 occupant 发送了事件：%#v", events)
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

func TestNotifierDeliveryFailureDoesNotCommitDedupeState(t *testing.T) {
	sink := &notifierIM{failAt: 1}
	notifier := mustNotifier(t, sink, nil)
	transition := notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)

	if err := notifier.HandleTransition(context.Background(), transition); err == nil {
		t.Fatal("首轮发送失败时 HandleTransition() 未返回错误")
	}
	if err := notifier.HandleTransition(context.Background(), transition); err != nil {
		t.Fatalf("重试 HandleTransition() 返回错误：%v", err)
	}
	if events := sink.Events(); len(events) != 2 || sink.CallCount() != 2 {
		t.Fatalf("失败后未重发事件：calls=%d events=%#v", sink.CallCount(), events)
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

func TestNotificationDispatcherLogsStatusRetryAndSuccess(t *testing.T) {
	im := &failOnceNotifierIM{failAt: 1}
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	retry := &notificationRetry{delay: 7 * time.Second}
	waiter := newNotificationRetryWaiter()
	logs := &lockedLogBuffer{}
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Backoff:  retry,
		Wait:     waiter.Wait,
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
		t.Fatalf("EnqueueStatus() 返回错误：%v", err)
	}
	wait := <-waiter.started
	output := logs.String()
	for _, want := range []string{"Agent 状态通知发送失败", "pane_id=pane-1", "current_status=working", "error_type=delivery", "reason=\"send failed\"", "retry_delay=7s"} {
		if !strings.Contains(output, want) {
			t.Fatalf("通知失败日志缺少 %q：\n%s", want, output)
		}
	}
	close(wait.release)
	awaitNotifierCondition(t, "状态通知发送成功日志", func() bool {
		return strings.Contains(logs.String(), "Agent 状态通知已发送")
	})

	dispatcher.EndEpoch(epoch)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func TestNotificationDispatcherLogsStatusReplacement(t *testing.T) {
	logs := &lockedLogBuffer{}
	notifier := mustNotifier(t, &notifierIM{}, (&notifierReader{}).ReadRecent)
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 1,
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
	})
	epoch := dispatcher.BeginEpoch()
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusWorking, herdr.AgentStatusDone)); err != nil {
		t.Fatalf("done EnqueueStatus() 返回错误：%v", err)
	}
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusDone, herdr.AgentStatusIdle)); err != nil {
		t.Fatalf("idle EnqueueStatus() 返回错误：%v", err)
	}

	output := logs.String()
	for _, want := range []string{"Agent 状态通知已取消", "pane_id=pane-1", "current_status=done", "replacement_status=idle"} {
		if !strings.Contains(output, want) {
			t.Fatalf("通知替换日志缺少 %q：\n%s", want, output)
		}
	}
}

func TestNotificationDispatcherPermanentInvalidationFailureDoesNotBlockStatus(t *testing.T) {
	im := newTargetChangedNotifierIM()
	notifier := mustNotifier(t, im, (&notifierReader{}).ReadRecent)
	retry := &notificationRetry{delay: time.Hour}
	logs := &lockedLogBuffer{}
	dispatcher := newNotificationDispatcher(notifier, notificationDispatcherOptions{
		Capacity: 2,
		Backoff:  retry,
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- dispatcher.Run(ctx) }()
	epoch := dispatcher.BeginEpoch()

	if err := dispatcher.EnqueueInvalidated(notificationTarget()); err != nil {
		t.Fatalf("EnqueueInvalidated() 返回错误：%v", err)
	}
	select {
	case <-im.invalidated:
	case <-time.After(3 * time.Second):
		t.Fatal("目标失效通知未开始发送")
	}
	if err := dispatcher.EnqueueStatus(epoch, notificationTransition(herdr.AgentStatusIdle, herdr.AgentStatusWorking)); err != nil {
		t.Fatalf("EnqueueStatus() 返回错误：%v", err)
	}
	awaitNotifierCondition(t, "永久错误后的状态通知", func() bool { return len(im.Messages()) == 1 })
	if retry.NextCount() != 0 {
		t.Fatalf("永久目标变化错误进入重试：Next=%d", retry.NextCount())
	}
	if output := logs.String(); !strings.Contains(output, "Agent 目标失效通知发送已停止") ||
		!strings.Contains(output, "error_type=target_changed") ||
		!strings.Contains(output, "error_reason=\"Agent 列表快照已过期\"") ||
		!strings.Contains(output, "retryable=false") {
		t.Fatalf("永久目标变化错误日志不完整：\n%s", output)
	}

	dispatcher.EndEpoch(epoch)
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

func mustNotifier(t *testing.T, im IMAdapter, read func(context.Context, string, int) (herdr.ReadResult, error)) *Notifier {
	t.Helper()
	return mustNotifierWithGetter(t, im, matchingNotificationAgent, read)
}

func mustNotifierWithGetter(t *testing.T, im IMAdapter, get GetAgentFunc, _ func(context.Context, string, int) (herdr.ReadResult, error)) *Notifier {
	t.Helper()
	notifier, err := NewNotifier(im, get)
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
	targets  []im.NotificationTarget
	events   []im.NotificationEvent
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

type targetChangedNotifierIM struct {
	mu          sync.Mutex
	messages    []string
	invalidated chan struct{}
	once        sync.Once
}

func newTargetChangedNotifierIM() *targetChangedNotifierIM {
	return &targetChangedNotifierIM{invalidated: make(chan struct{})}
}

func (i *targetChangedNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *targetChangedNotifierIM) SendMarkdown(_ context.Context, content string) error {
	if strings.Contains(content, "目标已失效") {
		i.once.Do(func() { close(i.invalidated) })
		return session.ErrListSnapshotExpired
	}
	i.mu.Lock()
	i.messages = append(i.messages, content)
	i.mu.Unlock()
	return nil
}

func (i *targetChangedNotifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
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
	return i.record(im.NotificationTarget{}, content)
}

func (i *notifierIM) SendNotification(_ context.Context, target im.NotificationTarget, event im.NotificationEvent) error {
	i.mu.Lock()
	i.events = append(i.events, event)
	i.mu.Unlock()
	return i.record(target, renderNotificationEvent(event, target))
}

func (i *notifierIM) record(target im.NotificationTarget, content string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	i.messages = append(i.messages, content)
	i.targets = append(i.targets, target)
	if i.failAt > 0 && i.calls == i.failAt {
		return errors.New("send failed")
	}
	return nil
}

func (i *notifierIM) Targets() []im.NotificationTarget {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]im.NotificationTarget(nil), i.targets...)
}

func (i *notifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
}

func (i *notifierIM) Events() []im.NotificationEvent {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]im.NotificationEvent(nil), i.events...)
}

func (i *notifierIM) CallCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

func (i *notifierIM) SetFailAt(call int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failAt = call
}
