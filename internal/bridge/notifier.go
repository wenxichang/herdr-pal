package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/session"
)

var (
	// ErrInvalidNotifierDependency 表示 Notifier 缺少必需依赖。
	ErrInvalidNotifierDependency = errors.New("Notifier 依赖无效")
	// ErrNotificationQueueFull 表示主动通知待发队列已达到有界容量。
	ErrNotificationQueueFull = errors.New("主动通知队列已满")
)

const defaultNotificationQueueCapacity = 64

// GetAgentFunc 查询目标当前的 Agent occupant，供通知发送前校验。
type GetAgentFunc func(ctx context.Context, target string) (herdr.AgentInfo, error)

type notificationKey struct {
	paneID         string
	occupantKey    string
	kind           string
	previousStatus herdr.AgentStatus
	status         herdr.AgentStatus
}

type notificationTaskKind uint8

const (
	notificationTaskStatus notificationTaskKind = iota
	notificationTaskInvalidated
)

type notificationTask struct {
	id              uint64
	kind            notificationTaskKind
	epoch           uint64
	paneID          string
	invalidationKey string
	transition      session.Transition
	target          session.Target
}

type notificationDispatcherOptions struct {
	Capacity int
	Backoff  RetryPolicy
	Wait     SupervisorWaitFunc
	Logger   *slog.Logger
}

// notificationDispatcher 在 Supervisor 整个 Run 生命周期内串行发送主动通知。
//
// 状态任务按 pane 合并且受连接周期 epoch 约束；目标失效任务不绑定 epoch，因此会跨
// Herdr 重连保留。队列锁只保护内存任务，不跨越终端读取、IM 发送或重试等待。
type notificationDispatcher struct {
	notifier *Notifier
	capacity int
	backoff  RetryPolicy
	wait     SupervisorWaitFunc
	logger   *slog.Logger
	wake     chan struct{}

	mu                 sync.Mutex
	nextTaskID         uint64
	nextEpoch          uint64
	currentEpoch       uint64
	queue              []*notificationTask
	pendingStatus      map[string]*notificationTask
	pendingInvalidated map[string]*notificationTask
	active             *notificationTask
	activeCancel       context.CancelFunc
}

func newNotificationDispatcher(notifier *Notifier, options notificationDispatcherOptions) *notificationDispatcher {
	if options.Capacity <= 0 {
		options.Capacity = defaultNotificationQueueCapacity
	}
	if options.Backoff == nil {
		options.Backoff = newDefaultSupervisorRetry(time.Second, 30*time.Second, nil)
	}
	if options.Wait == nil {
		options.Wait = waitSupervisorDelay
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &notificationDispatcher{
		notifier: notifier, capacity: options.Capacity, backoff: options.Backoff, wait: options.Wait, logger: options.Logger,
		wake: make(chan struct{}, 1), pendingStatus: make(map[string]*notificationTask),
		pendingInvalidated: make(map[string]*notificationTask),
	}
}

func (d *notificationDispatcher) BeginEpoch() uint64 {
	d.mu.Lock()
	d.nextEpoch++
	d.currentEpoch = d.nextEpoch
	d.dropStatusLocked(0)
	epoch := d.currentEpoch
	d.mu.Unlock()
	d.signal()
	return epoch
}

func (d *notificationDispatcher) EndEpoch(epoch uint64) {
	d.mu.Lock()
	if d.currentEpoch != epoch {
		d.mu.Unlock()
		return
	}
	d.currentEpoch = 0
	d.dropStatusLocked(epoch)
	d.mu.Unlock()
	d.signal()
}

func (d *notificationDispatcher) EnqueueStatus(epoch uint64, transition session.Transition) error {
	d.mu.Lock()
	if epoch == 0 || d.currentEpoch != epoch {
		d.mu.Unlock()
		d.logger.Warn("Agent 状态通知未入队", append(statusTransitionLogArgs(transition), "reason", "stale_epoch")...)
		return nil
	}
	paneID := transition.Target.PaneID
	dropped := d.dropPaneStatusLocked(paneID)
	if !notificationPolicy(transition) {
		d.mu.Unlock()
		d.logStatusReplacement(dropped, transition.Current)
		d.logger.Info("Agent 状态无需通知", statusTransitionLogArgs(transition)...)
		return nil
	}
	if len(d.queue) >= d.capacity {
		d.mu.Unlock()
		d.logStatusReplacement(dropped, transition.Current)
		d.logger.Error("Agent 状态通知未入队", append(statusTransitionLogArgs(transition), "reason", "queue_full")...)
		return ErrNotificationQueueFull
	}
	d.nextTaskID++
	task := &notificationTask{id: d.nextTaskID, kind: notificationTaskStatus, epoch: epoch, paneID: paneID, transition: transition}
	d.queue = append(d.queue, task)
	d.pendingStatus[paneID] = task
	d.mu.Unlock()
	d.logStatusReplacement(dropped, transition.Current)
	d.logger.Info("Agent 状态通知已入队", statusTransitionLogArgs(transition)...)
	d.signal()
	return nil
}

func (d *notificationDispatcher) EnqueueInvalidated(target session.Target) error {
	d.mu.Lock()
	dropped := d.dropPaneStatusLocked(target.PaneID)
	key := target.PaneID + "\x00" + target.OccupantKey
	if d.pendingInvalidated[key] != nil {
		d.mu.Unlock()
		d.logStatusCancellation(dropped, "target_invalidated", herdr.AgentStatusUnknown)
		return nil
	}
	if len(d.queue) >= d.capacity {
		d.mu.Unlock()
		d.logStatusCancellation(dropped, "target_invalidated", herdr.AgentStatusUnknown)
		return ErrNotificationQueueFull
	}
	d.nextTaskID++
	task := &notificationTask{
		id: d.nextTaskID, kind: notificationTaskInvalidated, paneID: target.PaneID,
		invalidationKey: key, target: target,
	}
	d.queue = append(d.queue, task)
	d.pendingInvalidated[key] = task
	d.mu.Unlock()
	d.logStatusCancellation(dropped, "target_invalidated", herdr.AgentStatusUnknown)
	d.signal()
	return nil
}

func (d *notificationDispatcher) Run(ctx context.Context) error {
	if d == nil || d.notifier == nil {
		return ErrInvalidNotifierDependency
	}
	for {
		task, taskContext, cancel, ok := d.next(ctx)
		if !ok {
			return ctx.Err()
		}
		err := d.deliver(taskContext, task)
		for err != nil && !isPermanentNotificationError(err) && taskContext.Err() == nil && d.taskCurrent(task) {
			delay := d.backoff.Next()
			d.logStatusDeliveryFailure(task, err, delay)
			if waitErr := d.wait(taskContext, delay); waitErr != nil {
				if taskContext.Err() != nil {
					break
				}
				d.logStatusDeliveryStopped(task, "retry_wait_failed")
				d.backoff.Reset()
				d.finish(task.id, cancel)
				return fmt.Errorf("主动通知重试等待失败: %w", waitErr)
			}
			if !d.taskCurrent(task) {
				break
			}
			err = d.deliver(taskContext, task)
		}
		permanentFailure := err != nil && isPermanentNotificationError(err)
		if permanentFailure {
			d.logPermanentNotificationFailure(task, err)
		}
		if task.kind == notificationTaskStatus {
			if err == nil {
				d.logger.Info("Agent 状态通知已发送", statusTransitionLogArgs(task.transition)...)
			} else if !permanentFailure {
				reason := "context_canceled"
				if ctx.Err() == nil {
					reason = "replaced_or_epoch_ended"
				}
				d.logStatusDeliveryStopped(task, reason)
			}
		}
		d.backoff.Reset()
		d.finish(task.id, cancel)
	}
}

func (d *notificationDispatcher) next(ctx context.Context) (*notificationTask, context.Context, context.CancelFunc, bool) {
	for {
		if ctx.Err() != nil {
			return nil, nil, nil, false
		}
		d.mu.Lock()
		if ctx.Err() != nil {
			d.mu.Unlock()
			return nil, nil, nil, false
		}
		if len(d.queue) > 0 {
			task := d.queue[0]
			d.queue = d.queue[1:]
			if task.kind == notificationTaskStatus && d.pendingStatus[task.paneID] == task {
				delete(d.pendingStatus, task.paneID)
			}
			if task.kind == notificationTaskStatus && task.epoch != d.currentEpoch {
				d.mu.Unlock()
				continue
			}
			taskContext, cancel := context.WithCancel(ctx)
			d.active = task
			d.activeCancel = cancel
			d.mu.Unlock()
			return task, taskContext, cancel, true
		}
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, nil, false
		case <-d.wake:
		}
	}
}

func (d *notificationDispatcher) deliver(ctx context.Context, task *notificationTask) error {
	switch task.kind {
	case notificationTaskStatus:
		return d.notifier.HandleTransition(ctx, task.transition)
	case notificationTaskInvalidated:
		return d.notifier.TargetInvalidated(ctx, task.target)
	default:
		return nil
	}
}

func (d *notificationDispatcher) taskCurrent(task *notificationTask) bool {
	if task.kind == notificationTaskInvalidated {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.currentEpoch == task.epoch && d.pendingStatus[task.paneID] == nil
}

func (d *notificationDispatcher) finish(taskID uint64, cancel context.CancelFunc) {
	cancel()
	d.mu.Lock()
	if d.active != nil && d.active.id == taskID {
		if d.active.kind == notificationTaskInvalidated && d.pendingInvalidated[d.active.invalidationKey] == d.active {
			delete(d.pendingInvalidated, d.active.invalidationKey)
		}
		d.active = nil
		d.activeCancel = nil
	}
	d.mu.Unlock()
}

func (d *notificationDispatcher) dropStatusLocked(epoch uint64) {
	kept := d.queue[:0]
	droppedPanes := make(map[string]struct{})
	clear(d.pendingStatus)
	for _, task := range d.queue {
		if task.kind == notificationTaskStatus && (epoch == 0 || task.epoch == epoch) {
			droppedPanes[task.paneID] = struct{}{}
			continue
		}
		kept = append(kept, task)
		if task.kind == notificationTaskStatus {
			d.pendingStatus[task.paneID] = task
		}
	}
	d.queue = kept
	if d.active != nil && d.active.kind == notificationTaskStatus && (epoch == 0 || d.active.epoch == epoch) {
		droppedPanes[d.active.paneID] = struct{}{}
		if d.activeCancel != nil {
			d.activeCancel()
		}
	}
	for paneID := range droppedPanes {
		d.notifier.discardStatus(paneID)
	}
}

func (d *notificationDispatcher) dropPaneStatusLocked(paneID string) *notificationTask {
	var dropped *notificationTask
	pending := d.pendingStatus[paneID]
	if pending != nil {
		dropped = pending
		kept := d.queue[:0]
		for _, task := range d.queue {
			if task != pending {
				kept = append(kept, task)
			}
		}
		d.queue = kept
		delete(d.pendingStatus, paneID)
	}
	if d.active != nil && d.active.kind == notificationTaskStatus && d.active.paneID == paneID && d.activeCancel != nil {
		dropped = d.active
		d.activeCancel()
	}
	d.notifier.discardStatus(paneID)
	return dropped
}

func (d *notificationDispatcher) logStatusReplacement(task *notificationTask, replacement herdr.AgentStatus) {
	d.logStatusCancellation(task, "replaced", replacement)
}

func (d *notificationDispatcher) logStatusCancellation(task *notificationTask, reason string, replacement herdr.AgentStatus) {
	if task == nil || task.kind != notificationTaskStatus {
		return
	}
	args := append(statusTransitionLogArgs(task.transition), "reason", reason)
	if replacement != herdr.AgentStatusUnknown {
		args = append(args, "replacement_status", replacement)
	}
	d.logger.Warn("Agent 状态通知已取消", args...)
}

func (d *notificationDispatcher) logStatusDeliveryFailure(task *notificationTask, err error, delay time.Duration) {
	if task.kind != notificationTaskStatus {
		return
	}
	d.logger.Warn("Agent 状态通知发送失败", append(
		statusTransitionLogArgs(task.transition),
		"error_type", notificationDeliveryErrorType(err),
		"reason", safeNotificationErrorReason(err),
		"retry_delay", delay,
	)...)
}

func (d *notificationDispatcher) logStatusDeliveryStopped(task *notificationTask, reason string) {
	if task.kind != notificationTaskStatus {
		return
	}
	d.logger.Warn("Agent 状态通知发送已停止", append(
		statusTransitionLogArgs(task.transition), "reason", reason,
	)...)
}

func (d *notificationDispatcher) logPermanentNotificationFailure(task *notificationTask, err error) {
	errorType := notificationDeliveryErrorType(err)
	switch task.kind {
	case notificationTaskStatus:
		d.logger.Warn("Agent 状态通知发送已停止", append(
			statusTransitionLogArgs(task.transition),
			"reason", "permanent_error",
			"error_type", errorType,
			"error_reason", safeNotificationErrorReason(err),
			"retryable", false,
		)...)
	case notificationTaskInvalidated:
		d.logger.Warn("Agent 目标失效通知发送已停止",
			"pane_id", task.target.PaneID,
			"occupant_hash", bridgeShortHash(task.target.OccupantKey),
			"agent", task.target.Agent,
			"error_type", errorType,
			"error_reason", safeNotificationErrorReason(err),
			"retryable", false,
		)
	}
}

func isPermanentNotificationError(err error) bool {
	return errors.Is(err, session.ErrListSnapshotExpired) || errors.Is(err, session.ErrSelectionInvalid)
}

func notificationDeliveryErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.Is(err, session.ErrListSnapshotExpired), errors.Is(err, session.ErrSelectionInvalid):
		return "target_changed"
	case errors.Is(err, herdr.ErrUnavailable):
		return "herdr_unavailable"
	default:
		return "delivery"
	}
}

func safeNotificationErrorReason(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.Join(strings.Fields(err.Error()), " ")
	const maxRunes = 512
	runes := []rune(reason)
	if len(runes) > maxRunes {
		reason = string(runes[:maxRunes]) + "..."
	}
	return reason
}

func (d *notificationDispatcher) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Notifier 根据 Agent 状态迁移发送轻量主动事件。
//
// Notifier 不读取终端快照；上游服务端可根据事件、用户活跃度和输出模式决定是否另行
// 请求内容。内部锁只保护事件去重元数据，不跨越 Herdr 查询或消息发送调用。
type Notifier struct {
	notification im.NotificationSink
	get          GetAgentFunc

	mu       sync.Mutex
	recent   map[string]notificationKey
	inflight map[string]chan struct{}
	epoch    uint64
}

// NewNotifier 创建状态通知器，并要求发送前可实时校验 Agent occupant。
func NewNotifier(adapter IMAdapter, get GetAgentFunc) (*Notifier, error) {
	if adapter == nil || get == nil {
		return nil, ErrInvalidNotifierDependency
	}
	notification, ok := adapter.(im.NotificationSink)
	if !ok {
		notification = replyNotificationSink{reply: adapter}
	}
	return &Notifier{
		notification: notification,
		get:          get,
		recent:       make(map[string]notificationKey),
		inflight:     make(map[string]chan struct{}),
	}, nil
}

type replyNotificationSink struct {
	reply im.ReplySink
}

func (s replyNotificationSink) SendNotification(ctx context.Context, target im.NotificationTarget, event im.NotificationEvent) error {
	content := renderNotificationEvent(event, target)
	for _, part := range panel.SplitMarkdown(content, panel.WeComContentLimit) {
		if err := s.reply.SendMarkdown(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

// Reset 清空当前进程内的通知去重基线，供 Herdr 重连后的新健康周期使用。
func (n *Notifier) Reset() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.epoch++
	n.recent = make(map[string]notificationKey)
	n.mu.Unlock()
}

// discardStatus 废弃指定 pane 的状态通知去重结果。
func (n *Notifier) discardStatus(paneID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if recent, exists := n.recent[paneID]; exists && recent.kind == "status" {
		delete(n.recent, paneID)
	}
	n.mu.Unlock()
}

// HandleTransition 根据状态迁移发送主动通知。
func (n *Notifier) HandleTransition(ctx context.Context, transition session.Transition) error {
	if n == nil {
		return ErrInvalidNotifierDependency
	}
	if !notificationPolicy(transition) {
		return nil
	}
	current, err := n.get(ctx, transition.Target.PaneID)
	if err != nil || !session.MatchesAgent(transition.Target, current) {
		return nil
	}

	key := notificationKey{
		paneID:         transition.Target.PaneID,
		occupantKey:    transition.Target.OccupantKey,
		kind:           "status",
		previousStatus: transition.Previous,
		status:         transition.Current,
	}
	event := im.NotificationEvent{
		Kind:           im.NotificationKindAgentStatusChanged,
		PreviousStatus: string(transition.Previous),
		Status:         string(transition.Current),
		OccurredAt:     time.Now().UTC(),
	}
	return n.deliverOnce(ctx, transition.Target, key, event)
}

// TargetInvalidated 通知 pane 关闭或 occupant 替换。
func (n *Notifier) TargetInvalidated(ctx context.Context, target session.Target) error {
	if n == nil {
		return ErrInvalidNotifierDependency
	}
	key := notificationKey{paneID: target.PaneID, occupantKey: target.OccupantKey, kind: "invalidated"}
	event := im.NotificationEvent{Kind: im.NotificationKindTargetInvalidated, OccurredAt: time.Now().UTC()}
	return n.deliverOnce(ctx, target, key, event)
}

func notificationPolicy(transition session.Transition) bool {
	if transition.Previous == transition.Current {
		return false
	}
	switch transition.Current {
	case herdr.AgentStatusWorking, herdr.AgentStatusBlocked, herdr.AgentStatusDone, herdr.AgentStatusUnknown:
		return true
	case herdr.AgentStatusIdle:
		return transition.Previous == herdr.AgentStatusWorking || transition.Previous == herdr.AgentStatusBlocked
	default:
		return false
	}
}

func (n *Notifier) deliverOnce(ctx context.Context, target session.Target, key notificationKey, event im.NotificationEvent) error {
	paneID := target.PaneID
	notificationTarget := im.NotificationTarget{
		PaneID:       target.PaneID,
		OccupantHash: target.OccupantKey,
		Agent:        target.Agent,
		DisplayAgent: target.DisplayAgent,
		Title:        target.Title,
	}
	for {
		n.mu.Lock()
		if n.recent[paneID] == key {
			n.mu.Unlock()
			return nil
		}
		if pending := n.inflight[paneID]; pending != nil {
			n.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending:
				continue
			}
		}
		completed := make(chan struct{})
		n.inflight[paneID] = completed
		deliveryEpoch := n.epoch
		n.mu.Unlock()

		err := n.notification.SendNotification(ctx, notificationTarget, event)

		n.mu.Lock()
		if err == nil && n.epoch == deliveryEpoch {
			n.recent[paneID] = key
		}
		delete(n.inflight, paneID)
		close(completed)
		n.mu.Unlock()
		return err
	}
}

func renderNotificationEvent(event im.NotificationEvent, target im.NotificationTarget) string {
	title := "Agent 状态发生变化。"
	switch event.Kind {
	case im.NotificationKindTargetInvalidated:
		title = "Agent 目标已失效，请重新执行 /ls 并使用 /N 选择目标。"
	case im.NotificationKindAgentStatusChanged:
		switch herdr.AgentStatus(event.Status) {
		case herdr.AgentStatusWorking:
			title = "Agent 开始工作。"
		case herdr.AgentStatusBlocked:
			title = "Agent 已阻塞，需要你的处理。"
		case herdr.AgentStatusDone:
			title = "Agent 已完成。"
		case herdr.AgentStatusIdle:
			title = "Agent 已空闲。"
		case herdr.AgentStatusUnknown:
			title = "Agent 状态无法可靠识别，请在 Herdr 中确认。"
		}
	}
	return renderStatusTitle(title, session.Target{
		PaneID: target.PaneID, OccupantKey: target.OccupantHash, Agent: target.Agent,
		DisplayAgent: target.DisplayAgent, Title: target.Title,
	})
}

func renderStatusTitle(title string, target session.Target) string {
	name := target.DisplayAgent
	if name == "" {
		name = target.Agent
	}
	if name == "" {
		name = target.PaneID
	}
	content := fmt.Sprintf("%s\n目标：%s（%s）", title, safeLabel(name), safeLabel(target.PaneID))
	if target.Title != "" {
		content += "\n标题：" + safeLabel(target.Title)
	}
	return content
}

func renderStatusTitleParts(title string, target session.Target) []string {
	return panel.SplitMarkdown(renderStatusTitle(title, target), panel.WeComContentLimit)
}
