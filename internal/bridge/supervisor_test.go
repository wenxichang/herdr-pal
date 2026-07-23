package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestSupervisorConnectsChecksSnapshotsAndBuildsBaselineSubscriptions(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(
		supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusDone),
		supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
	))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)

	first := awaitSupervisorSubscribe(t, client)
	second := awaitSupervisorSubscribe(t, client)
	if !reflect.DeepEqual(first, herdr.LifecycleSubscriptions()) {
		t.Fatalf("lifecycle specs = %#v", first)
	}
	wantStatus := herdr.StatusSubscriptions([]string{"pane-1", "pane-2"})
	if !reflect.DeepEqual(second, wantStatus) {
		t.Fatalf("status specs = %#v，期望 %#v", second, wantStatus)
	}
	awaitSupervisorCondition(t, "启动基线", func() bool { return serviceAvailable(harness.service) })
	if got, want := harness.log.Entries(), []string{"connect", "compatible", "snapshot", "subscribe:lifecycle", "subscribe:status", "snapshot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("启动调用顺序 = %#v，期望 %#v", got, want)
	}
	if messages := harness.im.Messages(); len(messages) != 0 {
		t.Fatalf("初始 snapshot 发送历史通知：%#v", messages)
	}
	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusDone))
	status.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusWorking))
	awaitSupervisorCondition(t, "基线重放抑制", func() bool { return len(harness.im.Messages()) == 1 })
	if message := harness.im.Messages()[0]; !strings.Contains(message, "开始工作") || strings.Contains(message, "已完成") {
		t.Fatalf("基线重放通知 = %q", message)
	}
	if !serviceAvailable(harness.service) {
		t.Fatal("订阅建立后 Service 未接管 Herdr client")
	}
}

func TestSupervisorPostSubscribeSnapshotIsAuthoritativeBaseline(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	discovery := supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking))
	postSubscribe := supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusDone))
	client := newSupervisorClient(discovery, postSubscribe)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "post-subscribe baseline", func() bool {
		targets := harness.registry.CreateListSnapshot()
		return serviceAvailable(harness.service) && len(targets) == 1 && targets[0].Status == herdr.AgentStatusDone
	})
	if messages := harness.im.Messages(); len(messages) != 0 {
		t.Fatalf("post-subscribe 基线发送历史通知：%#v", messages)
	}
}

func TestSupervisorInitialPaneSetReconcilesUntilStatusSubscriptionsStabilize(t *testing.T) {
	lifecycle := newSupervisorStream()
	firstStatus := newSupervisorStream()
	stableStatus := newSupervisorStream()
	discovery := supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking))
	changed := supervisorSnapshot(
		supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking),
		supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusDone),
	)
	client := newSupervisorClient(discovery, changed, changed)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{firstStatus, stableStatus}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	if specs := awaitSupervisorSubscribe(t, client); !reflect.DeepEqual(specs, herdr.StatusSubscriptions([]string{"pane-1"})) {
		t.Fatalf("首次 status specs = %#v", specs)
	}
	if specs := awaitSupervisorSubscribe(t, client); !reflect.DeepEqual(specs, herdr.StatusSubscriptions([]string{"pane-1", "pane-2"})) {
		t.Fatalf("稳定 status specs = %#v", specs)
	}
	awaitSupervisorCondition(t, "稳定 pane 集合基线", func() bool {
		return serviceAvailable(harness.service) && len(harness.registry.AgentPaneIDs()) == 2
	})
	if firstStatus.CloseCount() != 1 || client.SnapshotCount() != 3 || len(harness.im.Messages()) != 0 {
		t.Fatalf("pane 集合 reconcile 不完整：close=%d snapshots=%d messages=%#v", firstStatus.CloseCount(), client.SnapshotCount(), harness.im.Messages())
	}
}

func TestSupervisorStatusEventAppliesThenNotifiesAndSuppressesReplay(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	harness.reader.result = herdr.ReadResult{PaneID: "pane-1", Text: "需要确认"}

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "状态事件基线", func() bool { return serviceAvailable(harness.service) })

	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusBlocked))
	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusBlocked))
	awaitSupervisorCondition(t, "状态通知", func() bool { return len(harness.im.Messages()) >= 2 })

	targets := harness.registry.CreateListSnapshot()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusBlocked {
		t.Fatalf("Registry 状态 = %#v", targets)
	}
	if calls := harness.reader.Calls(); len(calls) != 1 || calls[0].lines != 100 {
		t.Fatalf("重复状态事件读取 = %#v", calls)
	}
	if messages := harness.im.Messages(); len(messages) != 2 || !strings.Contains(messages[0], "已阻塞") {
		t.Fatalf("重复状态事件通知 = %#v", messages)
	}
}

func TestSupervisorNotificationDeliveryDoesNotBlockStatusLoopAndCoalescesLatest(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(
		supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle),
		supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
	))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	im := newGatedNotifierIM()
	harness.supervisor.notifier = mustNotifierWithGetter(t, im, matchingSupervisorAgent, harness.reader.ReadRecent)

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "异步通知基线", func() bool { return serviceAvailable(harness.service) })

	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusWorking))
	<-im.firstStarted
	status.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusWorking))
	status.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusBlocked))
	status.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusDone))
	awaitSupervisorCondition(t, "阻塞发送期间处理最新状态", func() bool {
		for _, target := range harness.registry.CreateListSnapshot() {
			if target.PaneID == "pane-2" {
				return target.Status == herdr.AgentStatusDone
			}
		}
		return false
	})
	close(im.firstRelease)
	awaitSupervisorCondition(t, "异步状态通知", func() bool { return len(im.Messages()) == 2 })
	messages := strings.Join(im.Messages(), "\n")
	if !strings.Contains(messages, "开始工作") || !strings.Contains(messages, "已完成") || strings.Contains(messages, "已阻塞") {
		t.Fatalf("异步状态合并通知 = %q", messages)
	}
}

func TestSupervisorCycleEndDropsStatusButKeepsInvalidationDelivery(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	initial := supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle))
	empty := supervisorSnapshot()
	client := newSupervisorClient(initial, initial, empty, empty)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	im := newEpochNotifierIM()
	harness.supervisor.notifier = mustNotifierWithGetter(t, im, matchingSupervisorAgent, harness.reader.ReadRecent)

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "epoch 基线", func() bool { return serviceAvailable(harness.service) })
	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusWorking))
	<-im.firstStarted

	lifecycle.Emit(supervisorLifecycleEvent("pane.closed"))
	debounce := awaitSupervisorWait(t, harness.waiter)
	debounce.Release()
	awaitSupervisorCondition(t, "失效任务入队", func() bool {
		return client.SnapshotCount() == 4 && status.CloseCount() == 1 && len(harness.registry.AgentPaneIDs()) == 0
	})
	lifecycle.End(io.EOF)
	retry := awaitSupervisorWait(t, harness.waiter)
	if retry.delay != time.Second {
		t.Fatalf("周期结束重试等待 = %v，期望 1s", retry.delay)
	}
	awaitSupervisorCondition(t, "跨周期 invalidation 通知", func() bool { return len(im.Messages()) == 1 })
	if message := im.Messages()[0]; !strings.Contains(message, "目标已失效") || strings.Contains(message, "开始工作") {
		t.Fatalf("周期结束后的通知 = %q", message)
	}
}

func TestSupervisorNotificationQueueFullIsFatalAndClosesCycle(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(
		supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle),
		supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
		supervisorPane("pane-3", "terminal-3", "gemini", herdr.AgentStatusIdle),
	))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	im := newGatedNotifierIM()
	harness.supervisor.notifier = mustNotifierWithGetter(t, im, matchingSupervisorAgent, harness.reader.ReadRecent)
	harness.supervisor.notificationQueueCapacity = 1

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancel()
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "队列满测试基线", func() bool { return serviceAvailable(harness.service) })
	status.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusWorking))
	<-im.firstStarted
	status.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusWorking))
	awaitSupervisorCondition(t, "填满通知队列", func() bool {
		for _, target := range harness.registry.CreateListSnapshot() {
			if target.PaneID == "pane-2" {
				return target.Status == herdr.AgentStatusWorking
			}
		}
		return false
	})
	status.Emit(supervisorStatusEvent("pane-3", "workspace-1", "gemini", herdr.AgentStatusWorking))
	err := awaitSupervisorResult(t, result)
	if !errors.Is(err, ErrNotificationQueueFull) {
		t.Fatalf("Run() error = %v，期望 ErrNotificationQueueFull", err)
	}
	if serviceAvailable(harness.service) || lifecycle.CloseCount() != 1 || status.CloseCount() != 1 {
		t.Fatalf("队列满后未完整 degraded：available=%v lifecycleClose=%d statusClose=%d", serviceAvailable(harness.service), lifecycle.CloseCount(), status.CloseCount())
	}
	if harness.factory.ConnectCount() != 1 || harness.backoff.NextCount() != 0 || len(harness.waiter.requests) != 0 {
		t.Fatalf("队列满后发生重连：connect=%d backoff=%d waits=%d", harness.factory.ConnectCount(), harness.backoff.NextCount(), len(harness.waiter.requests))
	}
}

func TestSupervisorLifecycleEventsDebounceSnapshotAndRebuildStatusStream(t *testing.T) {
	lifecycle := newSupervisorStream()
	oldStatus := newSupervisorStream()
	newStatus := newSupervisorStream()
	client := newSupervisorClient(
		supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)),
		supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)),
		supervisorSnapshot(
			supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking),
			supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
		),
		supervisorSnapshot(
			supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking),
			supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
		),
	)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{oldStatus, newStatus}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	observed := make(chan supervisorMessage, 32)
	harness.supervisor.messageObserved = func(message supervisorMessage) {
		observed <- message
	}

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "生命周期基线", func() bool { return serviceAvailable(harness.service) })

	lifecycle.Emit(supervisorLifecycleEvent("pane.created"))
	firstDebounce := awaitSupervisorWait(t, harness.waiter)
	if firstDebounce.delay != 100*time.Millisecond {
		t.Fatalf("首次 debounce = %v", firstDebounce.delay)
	}
	lifecycle.Emit(supervisorLifecycleEvent("pane.updated"))
	secondDebounce := awaitSupervisorWait(t, harness.waiter)
	if secondDebounce.delay != 100*time.Millisecond {
		t.Fatalf("第二次 debounce = %v", secondDebounce.delay)
	}
	if err := awaitSupervisorWaitDone(t, firstDebounce); !errors.Is(err, context.Canceled) {
		t.Fatalf("旧 debounce 结果 = %v，期望 context.Canceled", err)
	}
	secondDebounce.Release()

	rebuilt := awaitSupervisorSubscribe(t, client)
	want := herdr.StatusSubscriptions([]string{"pane-1", "pane-2"})
	if !reflect.DeepEqual(rebuilt, want) {
		t.Fatalf("重建 status specs = %#v，期望 %#v", rebuilt, want)
	}
	awaitSupervisorCondition(t, "关闭旧 status stream", func() bool { return oldStatus.CloseCount() == 1 })
	if err := awaitSupervisorRecvEnded(t, oldStatus); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("旧 status Recv() 结束错误 = %v", err)
	}
	awaitSupervisorObservedMessage(t, observed, func(message supervisorMessage) bool {
		return message.kind == supervisorStreamStatus && message.generation == 1 && message.err != nil
	})
	newStatus.Emit(supervisorStatusEvent("pane-2", "workspace-1", "claude", herdr.AgentStatusWorking))
	awaitSupervisorCondition(t, "新 status stream 事件", func() bool {
		targets := harness.registry.CreateListSnapshot()
		return len(harness.im.Messages()) == 1 && len(targets) == 2 && targets[1].Status == herdr.AgentStatusWorking
	})
	if got := harness.factory.ConnectCount(); got != 1 {
		t.Fatalf("主动关闭旧 status stream 被误判为断线，Connect 次数 = %d", got)
	}
	if got := harness.backoff.NextCount(); got != 0 || len(harness.waiter.requests) != 0 {
		t.Fatalf("旧 generation 错误触发重连：backoff=%d waits=%d", got, len(harness.waiter.requests))
	}
	if got := client.SnapshotCount(); got != 4 {
		t.Fatalf("合并后的 snapshot 次数 = %d，期望 4（启动与生命周期各两次）", got)
	}
}

func TestSupervisorAllLifecycleKindsTriggerSnapshot(t *testing.T) {
	for _, kind := range []string{"pane.created", "pane.closed", "pane.updated", "pane.exited", "pane.agent_detected"} {
		t.Run(kind, func(t *testing.T) {
			lifecycle := newSupervisorStream()
			status := newSupervisorStream()
			client := newSupervisorClient(
				supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)),
				supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)),
			)
			client.lifecycleStreams = []*supervisorStream{lifecycle}
			client.statusStreams = []*supervisorStream{status}
			harness := newSupervisorHarness(t, []*supervisorClient{client})
			cancel, result := runSupervisor(t, harness.supervisor)
			defer cancelAndAwaitSupervisor(t, cancel, result)
			awaitSupervisorSubscribe(t, client)
			awaitSupervisorSubscribe(t, client)
			awaitSupervisorCondition(t, "生命周期基线", func() bool { return serviceAvailable(harness.service) })

			lifecycle.Emit(supervisorLifecycleEvent(kind))
			wait := awaitSupervisorWait(t, harness.waiter)
			wait.Release()
			awaitSupervisorCondition(t, "lifecycle snapshot", func() bool { return client.SnapshotCount() == 3 })
		})
	}
}

func TestSupervisorLifecycleReplacementInvalidatesOnlyAffectedSelectionAndNotifiesOldTargets(t *testing.T) {
	tests := []struct {
		name              string
		selectedIndex     int
		wantSelection     string
		wantPanelRetained bool
	}{
		{name: "affected selection", selectedIndex: 2, wantPanelRetained: false},
		{name: "unaffected selection", selectedIndex: 3, wantSelection: "pane-3", wantPanelRetained: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial := supervisorSnapshot(
				supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking),
				supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
				supervisorPane("pane-3", "terminal-3", "gemini", herdr.AgentStatusIdle),
			)
			next := supervisorSnapshot(
				supervisorPane("pane-2", "terminal-replaced", "codex", herdr.AgentStatusWorking),
				supervisorPane("pane-3", "terminal-3", "gemini", herdr.AgentStatusIdle),
			)
			lifecycle := newSupervisorStream()
			oldStatus := newSupervisorStream()
			newStatus := newSupervisorStream()
			client := newSupervisorClient(initial, initial, next, next)
			client.lifecycleStreams = []*supervisorStream{lifecycle}
			client.statusStreams = []*supervisorStream{oldStatus, newStatus}
			harness := newSupervisorHarness(t, []*supervisorClient{client})
			cancel, result := runSupervisor(t, harness.supervisor)
			defer cancelAndAwaitSupervisor(t, cancel, result)
			awaitSupervisorSubscribe(t, client)
			awaitSupervisorSubscribe(t, client)
			awaitSupervisorCondition(t, "替换场景基线", func() bool { return serviceAvailable(harness.service) })

			harness.registry.CreateListSnapshot()
			selected, err := harness.registry.Select(test.selectedIndex)
			if err != nil {
				t.Fatal(err)
			}
			harness.service.stateMu.Lock()
			harness.service.panel.Refresh(selected.OccupantKey, []string{"手工分页缓存"})
			harness.service.panelReady = true
			harness.service.page = 0
			harness.service.stateMu.Unlock()

			lifecycle.Emit(supervisorLifecycleEvent("pane.closed"))
			wait := awaitSupervisorWait(t, harness.waiter)
			wait.Release()
			awaitSupervisorSubscribe(t, client)
			awaitSupervisorCondition(t, "目标失效通知", func() bool { return len(harness.im.Messages()) == 2 })

			current, selectionErr := harness.registry.ValidateSelected()
			if test.wantSelection == "" {
				if selectionErr == nil {
					t.Fatalf("受影响选择仍然有效：%#v", current)
				}
			} else if selectionErr != nil || current.PaneID != test.wantSelection {
				t.Fatalf("未受影响选择被清理：target=%#v err=%v", current, selectionErr)
			}
			harness.service.stateMu.Lock()
			panelReady := harness.service.panelReady
			harness.service.stateMu.Unlock()
			if panelReady != test.wantPanelRetained {
				t.Fatalf("panelReady = %v，期望 %v", panelReady, test.wantPanelRetained)
			}
			messages := strings.Join(harness.im.Messages(), "\n")
			if !strings.Contains(messages, "pane-1") || !strings.Contains(messages, "pane-2") || strings.Contains(messages, "pane-3") {
				t.Fatalf("Removed/Replaced 失效通知 = %q", messages)
			}
		})
	}
}

func TestSupervisorCurrentStreamEndDegradesAndReconnectsFromFreshBaseline(t *testing.T) {
	firstLifecycle := newSupervisorStream()
	firstStatus := newSupervisorStream()
	secondLifecycle := newSupervisorStream()
	secondStatus := newSupervisorStream()
	first := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
	first.lifecycleStreams = []*supervisorStream{firstLifecycle}
	first.statusStreams = []*supervisorStream{firstStatus}
	second := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
	second.lifecycleStreams = []*supervisorStream{secondLifecycle}
	second.statusStreams = []*supervisorStream{secondStatus}
	harness := newSupervisorHarness(t, []*supervisorClient{first, second})
	harness.backoff.delays = []time.Duration{3 * time.Second}

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, first)
	awaitSupervisorSubscribe(t, first)
	awaitSupervisorCondition(t, "首次连接基线", func() bool { return serviceAvailable(harness.service) })
	firstStatus.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusWorking))
	awaitSupervisorCondition(t, "首次 working 通知", func() bool { return len(harness.im.Messages()) == 1 })
	harness.registry.CreateListSnapshot()
	selected, err := harness.registry.Select(1)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.stateMu.Lock()
	harness.service.panel.Refresh(selected.OccupantKey, []string{"旧分页"})
	harness.service.panelReady = true
	harness.service.stateMu.Unlock()

	firstStatus.End(io.EOF)
	retry := awaitSupervisorWait(t, harness.waiter)
	if retry.delay != 3*time.Second {
		t.Fatalf("重连退避 = %v，期望 3s", retry.delay)
	}
	awaitSupervisorCondition(t, "关闭两个失效 stream", func() bool {
		return firstLifecycle.CloseCount() == 1 && firstStatus.CloseCount() == 1
	})
	if serviceAvailable(harness.service) {
		t.Fatal("stream 结束后 Service 仍允许 Herdr 操作")
	}
	if _, err := harness.registry.ValidateSelected(); err == nil {
		t.Fatal("stream 结束后选择未清空")
	}
	harness.service.stateMu.Lock()
	panelReady := harness.service.panelReady
	harness.service.stateMu.Unlock()
	if panelReady {
		t.Fatal("stream 结束后手工分页缓存未清空")
	}

	retry.Release()
	awaitSupervisorSubscribe(t, second)
	awaitSupervisorSubscribe(t, second)
	awaitSupervisorCondition(t, "重连基线", func() bool { return serviceAvailable(harness.service) })
	if messages := harness.im.Messages(); len(messages) != 1 {
		t.Fatalf("重连 snapshot 重放历史通知：%#v", messages)
	}
	secondStatus.Emit(supervisorStatusEvent("pane-1", "workspace-1", "codex", herdr.AgentStatusWorking))
	awaitSupervisorCondition(t, "重连后的新状态通知", func() bool { return len(harness.im.Messages()) == 2 })
	if got := harness.factory.ConnectCount(); got != 2 {
		t.Fatalf("Connect 次数 = %d，期望 2", got)
	}
}

func TestSupervisorProtocolMismatchOnlyUsesSlowProbe(t *testing.T) {
	first := newSupervisorClient()
	first.checkErr = herdr.ErrProtocolMismatch
	second := newSupervisorClient()
	second.checkErr = herdr.ErrProtocolMismatch
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	compatible := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
	compatible.lifecycleStreams = []*supervisorStream{lifecycle}
	compatible.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{first, second, compatible})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	firstWait := awaitSupervisorWait(t, harness.waiter)
	if firstWait.delay != 30*time.Second {
		t.Fatalf("首次协议不匹配探测间隔 = %v，期望 30s", firstWait.delay)
	}
	if first.SnapshotCount() != 0 || first.SubscribeCount() != 0 {
		t.Fatalf("首次协议不匹配后仍调用业务 API：snapshot=%d subscribe=%d", first.SnapshotCount(), first.SubscribeCount())
	}
	if serviceAvailable(harness.service) {
		t.Fatal("协议不匹配时 Service 允许输入")
	}
	firstWait.Release()
	secondWait := awaitSupervisorWait(t, harness.waiter)
	if secondWait.delay != 30*time.Second {
		t.Fatalf("第二次协议不匹配探测间隔 = %v，期望 30s", secondWait.delay)
	}
	if second.SnapshotCount() != 0 || second.SubscribeCount() != 0 || harness.factory.ConnectCount() != 2 {
		t.Fatalf("第二次慢探测越过兼容检查：snapshot=%d subscribe=%d connect=%d", second.SnapshotCount(), second.SubscribeCount(), harness.factory.ConnectCount())
	}
	if harness.backoff.NextCount() != 0 {
		t.Fatalf("协议不匹配使用了普通退避：Next=%d", harness.backoff.NextCount())
	}
	secondWait.Release()
	awaitSupervisorSubscribe(t, compatible)
	awaitSupervisorSubscribe(t, compatible)
	awaitSupervisorCondition(t, "协议恢复基线", func() bool { return serviceAvailable(harness.service) })
	if compatible.SnapshotCount() != 2 || compatible.SubscribeCount() != 2 {
		t.Fatalf("协议恢复后未建立健康周期：available=%v snapshot=%d subscribe=%d", serviceAvailable(harness.service), compatible.SnapshotCount(), compatible.SubscribeCount())
	}
}

func TestSupervisorRejectsProtocolMismatchFromAnyInitialSnapshot(t *testing.T) {
	tests := []struct {
		name              string
		snapshots         []herdr.Snapshot
		lifecycleStreams  []*supervisorStream
		statusStreams     []*supervisorStream
		wantSubscribeCall int
	}{
		{
			name:      "discovery",
			snapshots: []herdr.Snapshot{supervisorSnapshotWithProtocol(18, supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle))},
		},
		{
			name: "post subscribe",
			snapshots: []herdr.Snapshot{
				supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)),
				supervisorSnapshotWithProtocol(18, supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)),
			},
			lifecycleStreams:  []*supervisorStream{newSupervisorStream()},
			statusStreams:     []*supervisorStream{newSupervisorStream()},
			wantSubscribeCall: 2,
		},
		{
			name: "reconcile post subscribe",
			snapshots: []herdr.Snapshot{
				supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)),
				supervisorSnapshot(
					supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle),
					supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
				),
				supervisorSnapshotWithProtocol(18,
					supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle),
					supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusIdle),
				),
			},
			lifecycleStreams: []*supervisorStream{newSupervisorStream()},
			statusStreams: []*supervisorStream{
				newSupervisorStream(),
				newSupervisorStream(),
			},
			wantSubscribeCall: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newSupervisorClient(test.snapshots...)
			client.lifecycleStreams = test.lifecycleStreams
			client.statusStreams = test.statusStreams
			harness := newSupervisorHarness(t, []*supervisorClient{client})
			cancel, result := runSupervisor(t, harness.supervisor)
			defer cancelAndAwaitSupervisor(t, cancel, result)
			wait := awaitSupervisorWait(t, harness.waiter)
			if wait.delay != 30*time.Second || harness.backoff.NextCount() != 0 {
				t.Fatalf("snapshot mismatch 未走慢探测：delay=%v backoff=%d", wait.delay, harness.backoff.NextCount())
			}
			if client.SubscribeCount() != test.wantSubscribeCall || serviceAvailable(harness.service) || len(harness.registry.AgentPaneIDs()) != 0 {
				t.Fatalf("snapshot mismatch 越过基线边界：subscribe=%d available=%v panes=%#v", client.SubscribeCount(), serviceAvailable(harness.service), harness.registry.AgentPaneIDs())
			}
			for _, stream := range append(append([]*supervisorStream(nil), test.lifecycleStreams...), test.statusStreams...) {
				if stream.CloseCount() != 1 {
					t.Fatalf("snapshot mismatch 后 stream Close 次数 = %d，期望 1", stream.CloseCount())
				}
			}
		})
	}
}

func TestSupervisorLifecycleSnapshotProtocolMismatchUsesSlowProbeWithoutReplacingBaseline(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	baseline := supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle))
	mismatch := supervisorSnapshotWithProtocol(18, supervisorPane("pane-2", "terminal-2", "claude", herdr.AgentStatusDone))
	client := newSupervisorClient(baseline, baseline, mismatch)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "lifecycle mismatch 基线", func() bool { return serviceAvailable(harness.service) })
	lifecycle.Emit(supervisorLifecycleEvent("pane.updated"))
	debounce := awaitSupervisorWait(t, harness.waiter)
	debounce.Release()
	retry := awaitSupervisorWait(t, harness.waiter)
	if retry.delay != 30*time.Second || harness.backoff.NextCount() != 0 {
		t.Fatalf("lifecycle snapshot mismatch 未走慢探测：delay=%v backoff=%d", retry.delay, harness.backoff.NextCount())
	}
	targets := harness.registry.CreateListSnapshot()
	if len(targets) != 1 || targets[0].PaneID != "pane-1" || serviceAvailable(harness.service) {
		t.Fatalf("lifecycle mismatch 污染基线：targets=%#v available=%v", targets, serviceAvailable(harness.service))
	}
}

func TestSupervisorOrdinaryFailuresUseBackoffAndStopAtFailedStep(t *testing.T) {
	tests := []struct {
		name    string
		clients func() []*supervisorClient
		wantLog []string
	}{
		{
			name:    "connect",
			clients: func() []*supervisorClient { return nil },
			wantLog: []string{"connect"},
		},
		{
			name: "compatible",
			clients: func() []*supervisorClient {
				client := newSupervisorClient()
				client.checkErr = errors.New("check failed")
				return []*supervisorClient{client}
			},
			wantLog: []string{"connect", "compatible"},
		},
		{
			name: "snapshot",
			clients: func() []*supervisorClient {
				client := newSupervisorClient()
				client.snapshots = []supervisorSnapshotResult{{err: errors.New("snapshot failed")}}
				return []*supervisorClient{client}
			},
			wantLog: []string{"connect", "compatible", "snapshot"},
		},
		{
			name: "lifecycle subscribe",
			clients: func() []*supervisorClient {
				client := newSupervisorClient(supervisorSnapshot())
				return []*supervisorClient{client}
			},
			wantLog: []string{"connect", "compatible", "snapshot", "subscribe:lifecycle"},
		},
		{
			name: "status subscribe",
			clients: func() []*supervisorClient {
				client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
				client.lifecycleStreams = []*supervisorStream{newSupervisorStream()}
				return []*supervisorClient{client}
			},
			wantLog: []string{"connect", "compatible", "snapshot", "subscribe:lifecycle", "subscribe:status"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newSupervisorHarness(t, test.clients())
			harness.backoff.delays = []time.Duration{4 * time.Second}
			cancel, result := runSupervisor(t, harness.supervisor)
			wait := awaitSupervisorWait(t, harness.waiter)
			if wait.delay != 4*time.Second {
				t.Fatalf("普通失败退避 = %v，期望 4s", wait.delay)
			}
			if got := harness.log.Entries(); !reflect.DeepEqual(got, test.wantLog) {
				t.Fatalf("失败调用顺序 = %#v，期望 %#v", got, test.wantLog)
			}
			if serviceAvailable(harness.service) {
				t.Fatal("普通失败后 Service 未 degraded")
			}
			cancel()
			if err := awaitSupervisorResult(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v，期望 context.Canceled", err)
			}
		})
	}
}

func TestSupervisorZeroAgentSnapshotDoesNotSubscribeEmptyStatusStream(t *testing.T) {
	lifecycle := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot())
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	if specs := awaitSupervisorSubscribe(t, client); !reflect.DeepEqual(specs, herdr.LifecycleSubscriptions()) {
		t.Fatalf("唯一订阅 = %#v", specs)
	}
	awaitSupervisorCondition(t, "零 Agent 启动完成", func() bool { return serviceAvailable(harness.service) })
	if got := client.SubscribeCount(); got != 1 {
		t.Fatalf("零 Agent 时 Subscribe 次数 = %d，期望仅 lifecycle 一次", got)
	}
}

func TestSupervisorLifecycleRemovalToZeroDoesNotSubscribeEmptyStatusStream(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(
		supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)),
		supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)),
		supervisorSnapshot(),
		supervisorSnapshot(),
	)
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "移除场景基线", func() bool { return serviceAvailable(harness.service) })
	lifecycle.Emit(supervisorLifecycleEvent("pane.closed"))
	wait := awaitSupervisorWait(t, harness.waiter)
	wait.Release()
	awaitSupervisorCondition(t, "移除最后一个 Agent", func() bool {
		return client.SnapshotCount() == 4 && status.CloseCount() == 1
	})
	if got := client.SubscribeCount(); got != 2 {
		t.Fatalf("移除到零 Agent 后 Subscribe 次数 = %d，期望不新增空 status 订阅", got)
	}
}

func TestSupervisorContextCancellationClosesBothStreams(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})

	cancel, result := runSupervisor(t, harness.supervisor)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	cancel()
	if err := awaitSupervisorResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
	if lifecycle.CloseCount() != 1 || status.CloseCount() != 1 {
		t.Fatalf("context 取消后的 Close 次数：lifecycle=%d status=%d", lifecycle.CloseCount(), status.CloseCount())
	}
}

func TestDefaultSupervisorRetryGrowsCapsAndResets(t *testing.T) {
	retry := newDefaultSupervisorRetry(time.Second, 3*time.Second, func() float64 { return 0.5 })
	if got := []time.Duration{retry.Next(), retry.Next(), retry.Next(), retry.Next()}; !reflect.DeepEqual(got, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second}) {
		t.Fatalf("默认退避 = %#v", got)
	}
	retry.Reset()
	if got := retry.Next(); got != time.Second {
		t.Fatalf("Reset 后退避 = %v", got)
	}
}

func TestDefaultSupervisorRetryClampsJitterToConfiguredBounds(t *testing.T) {
	tests := []struct {
		name   string
		random float64
		want   time.Duration
	}{
		{name: "zero", random: 0, want: time.Second},
		{name: "one", random: 1, want: 1200 * time.Millisecond},
		{name: "negative", random: -1, want: time.Second},
		{name: "above one", random: 2, want: 1200 * time.Millisecond},
		{name: "nan", random: math.NaN(), want: 1200 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retry := newDefaultSupervisorRetry(time.Second, 30*time.Second, func() float64 { return test.random })
			if got := retry.Next(); got != test.want {
				t.Fatalf("首次退避 = %v，期望 %v", got, test.want)
			}
		})
	}
	configured := newDefaultSupervisorRetry(2*time.Second, 5*time.Second, func() float64 { return 0 })
	if got := configured.Next(); got != 2*time.Second {
		t.Fatalf("自定义 minimum 未生效：%v", got)
	}

	retry := newDefaultSupervisorRetry(time.Second, 30*time.Second, func() float64 { return 1 })
	for attempt := 0; attempt < 20; attempt++ {
		delay := retry.Next()
		if delay < time.Second || delay > 30*time.Second {
			t.Fatalf("第 %d 次退避越界：%v", attempt+1, delay)
		}
	}
}

func TestWaitSupervisorDelayStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitSupervisorDelay(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitSupervisorDelay() error = %v", err)
	}
}

func TestSupervisorBackoffResetsOnlyAfterStableHealthyWindow(t *testing.T) {
	clients := make([]*supervisorClient, 0, 4)
	for index := 0; index < 4; index++ {
		lifecycle := newSupervisorStream()
		status := newSupervisorStream()
		if index < 3 {
			status.End(io.EOF)
		}
		client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusIdle)))
		client.lifecycleStreams = []*supervisorStream{lifecycle}
		client.statusStreams = []*supervisorStream{status}
		clients = append(clients, client)
	}
	harness := newSupervisorHarness(t, clients)
	clock := newSupervisorClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	harness.supervisor.now = clock.Now
	harness.supervisor.stableWindow = 30 * time.Second
	harness.supervisor.backoff = newDefaultSupervisorRetry(time.Second, 30*time.Second, func() float64 { return 0.5 })

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	for index, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		awaitSupervisorSubscribe(t, clients[index])
		awaitSupervisorSubscribe(t, clients[index])
		wait := awaitSupervisorWait(t, harness.waiter)
		if wait.delay != want {
			t.Fatalf("第 %d 个短周期退避 = %v，期望 %v", index+1, wait.delay, want)
		}
		wait.Release()
	}
	awaitSupervisorSubscribe(t, clients[3])
	awaitSupervisorSubscribe(t, clients[3])
	awaitSupervisorCondition(t, "稳定周期基线", func() bool { return serviceAvailable(harness.service) })
	clock.Advance(30 * time.Second)
	clients[3].statusStreams[0].End(io.EOF)
	wait := awaitSupervisorWait(t, harness.waiter)
	if wait.delay != time.Second {
		t.Fatalf("稳定周期后的退避 = %v，期望重置为 1s", wait.delay)
	}
}

func TestSupervisorMalformedStatusEndsHealthyCycle(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	harness.backoff.delays = []time.Duration{time.Second}

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "严格解码基线", func() bool { return serviceAvailable(harness.service) })
	status.Emit(herdr.Event{Kind: "pane.agent_status_changed", Data: json.RawMessage(`{"pane_id":"pane-1"}`)})

	wait := awaitSupervisorWait(t, harness.waiter)
	if wait.delay != time.Second {
		t.Fatalf("严格解码失败后的退避 = %v", wait.delay)
	}
	if serviceAvailable(harness.service) {
		t.Fatal("严格解码失败后 Service 未 degraded")
	}
}

func TestSupervisorLifecycleStreamEndAlsoClosesStatusStream(t *testing.T) {
	lifecycle := newSupervisorStream()
	status := newSupervisorStream()
	client := newSupervisorClient(supervisorSnapshot(supervisorPane("pane-1", "terminal-1", "codex", herdr.AgentStatusWorking)))
	client.lifecycleStreams = []*supervisorStream{lifecycle}
	client.statusStreams = []*supervisorStream{status}
	harness := newSupervisorHarness(t, []*supervisorClient{client})
	harness.backoff.delays = []time.Duration{2 * time.Second}

	cancel, result := runSupervisor(t, harness.supervisor)
	defer cancelAndAwaitSupervisor(t, cancel, result)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorSubscribe(t, client)
	awaitSupervisorCondition(t, "生命周期流基线", func() bool { return serviceAvailable(harness.service) })
	lifecycle.End(io.EOF)
	awaitSupervisorWait(t, harness.waiter)
	awaitSupervisorCondition(t, "关闭 status stream", func() bool { return status.CloseCount() == 1 })
}

type supervisorHarness struct {
	supervisor *Supervisor
	registry   *session.Registry
	service    *Service
	im         *notifierIM
	reader     *notifierReader
	factory    *supervisorFactory
	waiter     *supervisorWaiter
	backoff    *supervisorBackoff
	log        *supervisorCallLog
}

type epochNotifierIM struct {
	mu           sync.Mutex
	calls        int
	messages     []string
	firstStarted chan struct{}
}

func newEpochNotifierIM() *epochNotifierIM {
	return &epochNotifierIM{firstStarted: make(chan struct{})}
}

func (i *epochNotifierIM) RespondMarkdown(context.Context, string, string) error {
	return errors.New("Notifier 不应回复入站回调")
}

func (i *epochNotifierIM) SendMarkdown(ctx context.Context, content string) error {
	i.mu.Lock()
	i.calls++
	call := i.calls
	i.mu.Unlock()
	if call == 1 {
		close(i.firstStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	i.mu.Lock()
	i.messages = append(i.messages, content)
	i.mu.Unlock()
	return nil
}

func (i *epochNotifierIM) Messages() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.messages...)
}

func newSupervisorHarness(t *testing.T, clients []*supervisorClient) *supervisorHarness {
	t.Helper()
	registry := &session.Registry{}
	guard, err := policy.NewGuard("user-1")
	if err != nil {
		t.Fatal(err)
	}
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	im := &notifierIM{}
	service, err := NewService(registry, &panel.Buffer{}, guard, deduper, im)
	if err != nil {
		t.Fatal(err)
	}
	reader := &notifierReader{}
	notifier := mustNotifierWithGetter(t, im, matchingSupervisorAgent, reader.ReadRecent)
	log := &supervisorCallLog{}
	for _, client := range clients {
		client.log = log
	}
	factory := &supervisorFactory{clients: clients, connected: make(chan struct{}, 16), log: log}
	waiter := newSupervisorWaiter()
	backoff := &supervisorBackoff{delays: []time.Duration{time.Second}}
	supervisor, err := NewSupervisor(factory, registry, service, notifier, SupervisorOptions{
		Backoff:               backoff,
		Wait:                  waiter.Wait,
		DebounceDelay:         100 * time.Millisecond,
		ProtocolProbeInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() 返回错误：%v", err)
	}
	return &supervisorHarness{supervisor: supervisor, registry: registry, service: service, im: im, reader: reader, factory: factory, waiter: waiter, backoff: backoff, log: log}
}

func matchingSupervisorAgent(_ context.Context, target string) (herdr.AgentInfo, error) {
	return herdr.AgentInfo{
		PaneID: "pane-1", TerminalID: target, Agent: stringRef("codex"), DisplayAgent: stringRef("Codex"),
	}, nil
}

func runSupervisor(t *testing.T, supervisor *Supervisor) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	return cancel, result
}

func cancelAndAwaitSupervisor(t *testing.T, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	cancel()
	if err := awaitSupervisorResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v，期望 context.Canceled", err)
	}
}

func awaitSupervisorResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("等待 Supervisor 退出超时")
		return nil
	}
}

func awaitSupervisorSubscribe(t *testing.T, client *supervisorClient) []herdr.SubscriptionSpec {
	t.Helper()
	select {
	case specs := <-client.subscribed:
		return specs
	case <-time.After(3 * time.Second):
		t.Fatal("等待 Subscribe 调用超时")
		return nil
	}
}

func awaitSupervisorCondition(t *testing.T, name string, condition func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatalf("等待%s超时", name)
		default:
			runtime.Gosched()
		}
	}
}

func serviceAvailable(service *Service) bool {
	client, release, ok := service.beginOperation(false)
	if ok {
		release()
	}
	return ok && client != nil
}

func supervisorSnapshot(panes ...herdr.Pane) herdr.Snapshot {
	return supervisorSnapshotWithProtocol(herdr.RequiredProtocol, panes...)
}

func supervisorSnapshotWithProtocol(protocol uint32, panes ...herdr.Pane) herdr.Snapshot {
	return herdr.Snapshot{
		Version:    "0.1.0",
		Protocol:   protocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "workspace-1"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "tab-1"}},
		Panes:      panes,
	}
}

type supervisorClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSupervisorClock(now time.Time) *supervisorClock {
	return &supervisorClock{now: now}
}

func (c *supervisorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *supervisorClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func supervisorPane(paneID, terminalID, agent string, status herdr.AgentStatus) herdr.Pane {
	display := strings.ToUpper(agent[:1]) + agent[1:]
	return herdr.Pane{PaneID: paneID, TerminalID: terminalID, WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef(agent), DisplayAgent: stringRef(display), AgentStatus: status}
}

func supervisorStatusEvent(paneID, workspaceID, agent string, status herdr.AgentStatus) herdr.Event {
	data, _ := json.Marshal(map[string]any{
		"pane_id": paneID, "workspace_id": workspaceID, "agent_status": status, "agent": agent,
	})
	return herdr.Event{Kind: "pane.agent_status_changed", Data: data}
}

func supervisorLifecycleEvent(kind string) herdr.Event {
	return herdr.Event{Kind: kind, Data: json.RawMessage(`{"pane_id":"pane-1"}`)}
}

type supervisorCallLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *supervisorCallLog) Add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *supervisorCallLog) Entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

type supervisorFactory struct {
	mu        sync.Mutex
	clients   []*supervisorClient
	index     int
	calls     int
	connected chan struct{}
	log       *supervisorCallLog
}

func (f *supervisorFactory) Connect(context.Context) (ManagedHerdr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.log.Add("connect")
	select {
	case f.connected <- struct{}{}:
	default:
	}
	if f.index >= len(f.clients) {
		return nil, errors.New("没有更多 fake client")
	}
	client := f.clients[f.index]
	f.index++
	return client, nil
}

func (f *supervisorFactory) ConnectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type supervisorSnapshotResult struct {
	snapshot herdr.Snapshot
	err      error
}

type supervisorClient struct {
	mu               sync.Mutex
	log              *supervisorCallLog
	checkErr         error
	snapshots        []supervisorSnapshotResult
	snapshotIndex    int
	snapshotCalls    int
	lifecycleStreams []*supervisorStream
	lifecycleIndex   int
	statusStreams    []*supervisorStream
	statusIndex      int
	subscribeCalls   int
	subscribed       chan []herdr.SubscriptionSpec
}

func newSupervisorClient(snapshots ...herdr.Snapshot) *supervisorClient {
	results := make([]supervisorSnapshotResult, len(snapshots))
	for index, snapshot := range snapshots {
		results[index] = supervisorSnapshotResult{snapshot: snapshot}
	}
	return &supervisorClient{snapshots: results, subscribed: make(chan []herdr.SubscriptionSpec, 16)}
}

func (c *supervisorClient) CheckCompatible(context.Context) error {
	c.log.Add("compatible")
	return c.checkErr
}

func (c *supervisorClient) Snapshot(context.Context) (herdr.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.Add("snapshot")
	c.snapshotCalls++
	if len(c.snapshots) == 0 {
		return herdr.Snapshot{}, errors.New("没有更多 snapshot")
	}
	index := c.snapshotIndex
	if index >= len(c.snapshots) {
		index = len(c.snapshots) - 1
	} else {
		c.snapshotIndex++
	}
	result := c.snapshots[index]
	return result.snapshot, result.err
}

func (c *supervisorClient) Subscribe(_ context.Context, specs []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribeCalls++
	copySpecs := append([]herdr.SubscriptionSpec(nil), specs...)
	c.subscribed <- copySpecs
	if len(specs) > 0 && specs[0].Type == "pane.agent_status_changed" {
		c.log.Add("subscribe:status")
		if c.statusIndex >= len(c.statusStreams) {
			return nil, errors.New("没有更多 status stream")
		}
		stream := c.statusStreams[c.statusIndex]
		c.statusIndex++
		return stream, nil
	}
	c.log.Add("subscribe:lifecycle")
	if c.lifecycleIndex >= len(c.lifecycleStreams) {
		return nil, errors.New("没有更多 lifecycle stream")
	}
	stream := c.lifecycleStreams[c.lifecycleIndex]
	c.lifecycleIndex++
	return stream, nil
}

func (c *supervisorClient) GetAgent(context.Context, string) (herdr.AgentInfo, error) {
	return herdr.AgentInfo{}, nil
}

func (c *supervisorClient) ReadRecent(context.Context, string, int) (herdr.ReadResult, error) {
	return herdr.ReadResult{}, nil
}

func (c *supervisorClient) Prompt(context.Context, string, string) error { return nil }

func (c *supervisorClient) SendKey(context.Context, string, string) error { return nil }

func (c *supervisorClient) SnapshotCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotCalls
}

func (c *supervisorClient) SubscribeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscribeCalls
}

type supervisorStreamItem struct {
	event herdr.Event
	err   error
}

type supervisorStream struct {
	items     chan supervisorStreamItem
	closed    chan struct{}
	recvEnded chan error
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func newSupervisorStream() *supervisorStream {
	return &supervisorStream{items: make(chan supervisorStreamItem, 32), closed: make(chan struct{}), recvEnded: make(chan error, 8)}
}

func (s *supervisorStream) Recv(ctx context.Context) (event herdr.Event, err error) {
	defer func() {
		if err != nil {
			select {
			case s.recvEnded <- err:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return herdr.Event{}, ctx.Err()
	case <-s.closed:
		return herdr.Event{}, io.ErrClosedPipe
	case item := <-s.items:
		return item.event, item.err
	}
}

func awaitSupervisorRecvEnded(t *testing.T, stream *supervisorStream) error {
	t.Helper()
	select {
	case err := <-stream.recvEnded:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("等待 SubscriptionStream.Recv 结束超时")
		return nil
	}
}

func awaitSupervisorObservedMessage(t *testing.T, observed <-chan supervisorMessage, match func(supervisorMessage) bool) supervisorMessage {
	t.Helper()
	for {
		select {
		case message := <-observed:
			if match(message) {
				return message
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待 Supervisor 主循环观察消息超时")
			return supervisorMessage{}
		}
	}
}

func (s *supervisorStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closes++
		s.mu.Unlock()
		close(s.closed)
	})
	return nil
}

func (s *supervisorStream) Emit(event herdr.Event) {
	s.items <- supervisorStreamItem{event: event}
}

func (s *supervisorStream) End(err error) {
	s.items <- supervisorStreamItem{err: err}
}

func (s *supervisorStream) CloseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

type supervisorWaitRequest struct {
	delay   time.Duration
	release chan struct{}
	done    chan error
	once    sync.Once
}

func (r *supervisorWaitRequest) Release() {
	r.once.Do(func() { close(r.release) })
}

type supervisorWaiter struct {
	requests chan *supervisorWaitRequest
}

func newSupervisorWaiter() *supervisorWaiter {
	return &supervisorWaiter{requests: make(chan *supervisorWaitRequest, 64)}
}

func (w *supervisorWaiter) Wait(ctx context.Context, delay time.Duration) error {
	request := &supervisorWaitRequest{delay: delay, release: make(chan struct{}), done: make(chan error, 1)}
	select {
	case w.requests <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	var err error
	select {
	case <-request.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	request.done <- err
	return err
}

func awaitSupervisorWait(t *testing.T, waiter *supervisorWaiter) *supervisorWaitRequest {
	t.Helper()
	select {
	case request := <-waiter.requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("等待可控 Wait 调用超时")
		return nil
	}
}

func awaitSupervisorWaitDone(t *testing.T, request *supervisorWaitRequest) error {
	t.Helper()
	select {
	case err := <-request.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("等待可控 Wait 结束超时")
		return nil
	}
}

type supervisorBackoff struct {
	mu       sync.Mutex
	delays   []time.Duration
	next     int
	resets   int
	nextCall int
}

func (b *supervisorBackoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextCall++
	if b.next >= len(b.delays) {
		return time.Second
	}
	delay := b.delays[b.next]
	b.next++
	return delay
}

func (b *supervisorBackoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resets++
}

func (b *supervisorBackoff) NextCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextCall
}
