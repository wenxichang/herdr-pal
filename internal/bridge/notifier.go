package bridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
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

// ReadRecentFunc 读取目标的 recent_unwrapped 终端快照。
type ReadRecentFunc func(ctx context.Context, target string, lines int) (herdr.ReadResult, error)

// GetAgentFunc 查询目标当前的 Agent occupant，供自动快照读取前后校验。
type GetAgentFunc func(ctx context.Context, target string) (herdr.AgentInfo, error)

type notificationKey struct {
	paneID            string
	occupantKey       string
	kind              string
	status            herdr.AgentStatus
	snapshotHash      string
	snapshotAvailable bool
}

type notificationProgress struct {
	key   notificationKey
	parts []string
	next  int
	epoch uint64
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
	return &notificationDispatcher{
		notifier: notifier, capacity: options.Capacity, backoff: options.Backoff, wait: options.Wait,
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
		return nil
	}
	paneID := transition.Target.PaneID
	d.dropPaneStatusLocked(paneID)
	if _, _, notify := notificationPolicy(transition); !notify {
		d.mu.Unlock()
		return nil
	}
	if len(d.queue) >= d.capacity {
		d.mu.Unlock()
		return ErrNotificationQueueFull
	}
	d.nextTaskID++
	task := &notificationTask{id: d.nextTaskID, kind: notificationTaskStatus, epoch: epoch, paneID: paneID, transition: transition}
	d.queue = append(d.queue, task)
	d.pendingStatus[paneID] = task
	d.mu.Unlock()
	d.signal()
	return nil
}

func (d *notificationDispatcher) EnqueueInvalidated(target session.Target) error {
	d.mu.Lock()
	d.dropPaneStatusLocked(target.PaneID)
	key := target.PaneID + "\x00" + target.OccupantKey
	if d.pendingInvalidated[key] != nil {
		d.mu.Unlock()
		return nil
	}
	if len(d.queue) >= d.capacity {
		d.mu.Unlock()
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
		for err != nil && taskContext.Err() == nil && d.taskCurrent(task) {
			if waitErr := d.wait(taskContext, d.backoff.Next()); waitErr != nil {
				if taskContext.Err() != nil {
					break
				}
				d.backoff.Reset()
				d.finish(task.id, cancel)
				return fmt.Errorf("主动通知重试等待失败: %w", waitErr)
			}
			if !d.taskCurrent(task) {
				break
			}
			err = d.deliver(taskContext, task)
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

func (d *notificationDispatcher) dropPaneStatusLocked(paneID string) {
	pending := d.pendingStatus[paneID]
	if pending != nil {
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
		d.activeCancel()
	}
	d.notifier.discardStatus(paneID)
}

func (d *notificationDispatcher) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Notifier 根据 Agent 状态迁移向企业微信发送主动通知。
//
// 自动快照只保留独立的通知去重状态，不读取或修改手工 PanelBuffer。内部锁只保护去重
// 元数据，不跨越 Herdr 读取或企业微信发送调用。
type Notifier struct {
	im   IMAdapter
	get  GetAgentFunc
	read ReadRecentFunc

	mu       sync.Mutex
	recent   map[string]notificationKey
	inflight map[string]chan struct{}
	pending  map[string]*notificationProgress
	epoch    uint64
}

// NewNotifier 创建状态通知器，并要求自动读取前后可实时校验 Agent occupant。
func NewNotifier(im IMAdapter, get GetAgentFunc, read ReadRecentFunc) (*Notifier, error) {
	if im == nil || get == nil || read == nil {
		return nil, ErrInvalidNotifierDependency
	}
	return &Notifier{
		im:       im,
		get:      get,
		read:     read,
		recent:   make(map[string]notificationKey),
		inflight: make(map[string]chan struct{}),
		pending:  make(map[string]*notificationProgress),
	}, nil
}

// Reset 清空当前进程内的通知去重基线，供 Herdr 重连后的新健康周期使用。
func (n *Notifier) Reset() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.epoch++
	n.recent = make(map[string]notificationKey)
	pending := make(map[string]*notificationProgress)
	for paneID, progress := range n.pending {
		if progress.key.kind == "invalidated" {
			pending[paneID] = progress
		}
	}
	n.pending = pending
	n.mu.Unlock()
}

// discardStatus 废弃指定 pane 的状态通知进度和去重结果。
//
// inflight 任务由 Dispatcher 的 context 取消；其后续更新会因 pending 指针变化失效。
// invalidation 进度和去重结果不属于状态迁移，必须跨抢占与重连保留。
func (n *Notifier) discardStatus(paneID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if progress := n.pending[paneID]; progress != nil && progress.key.kind == "status" {
		delete(n.pending, paneID)
	}
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
	title, includeSnapshot, notify := notificationPolicy(transition)
	if !notify {
		return nil
	}

	key := notificationKey{
		paneID:      transition.Target.PaneID,
		occupantKey: transition.Target.OccupantKey,
		kind:        "status",
		status:      transition.Current,
	}
	parts := renderStatusTitleParts(title, transition.Target)
	if includeSnapshot {
		before, err := n.get(ctx, transition.Target.TerminalID)
		if err == nil && session.MatchesAgent(transition.Target, before) {
			result, readErr := n.read(ctx, transition.Target.TerminalID, panel.PageSize)
			after, getErr := n.get(ctx, transition.Target.TerminalID)
			if readErr == nil && getErr == nil && session.MatchesAgent(transition.Target, after) && result.PaneID == transition.Target.PaneID {
				lines := lastNotificationLines(panel.Normalize(result.Text))
				key.snapshotAvailable = true
				key.snapshotHash = notificationHash(lines)
				parts = append(parts, renderNotificationSnapshot(transition.Target, lines)...)
			}
		}
	}
	return n.deliverOnce(ctx, transition.Target.PaneID, key, parts)
}

// TargetInvalidated 通知 pane 关闭或 occupant 替换。
func (n *Notifier) TargetInvalidated(ctx context.Context, target session.Target) error {
	if n == nil {
		return ErrInvalidNotifierDependency
	}
	key := notificationKey{paneID: target.PaneID, occupantKey: target.OccupantKey, kind: "invalidated"}
	return n.deliverOnce(ctx, target.PaneID, key, renderStatusTitleParts("Agent 目标已失效，请重新执行 /ls 和 /sel。", target))
}

func notificationPolicy(transition session.Transition) (title string, includeSnapshot, notify bool) {
	if transition.Previous == transition.Current {
		return "", false, false
	}
	switch transition.Current {
	case herdr.AgentStatusWorking:
		return "Agent 开始工作。", false, true
	case herdr.AgentStatusBlocked:
		return "Agent 已阻塞，需要你的处理。", true, true
	case herdr.AgentStatusDone:
		return "Agent 已完成。", true, true
	case herdr.AgentStatusIdle:
		if transition.Previous == herdr.AgentStatusWorking || transition.Previous == herdr.AgentStatusBlocked {
			return "Agent 已空闲。", true, true
		}
		return "", false, false
	case herdr.AgentStatusUnknown:
		return "Agent 状态无法可靠识别，请在 Herdr 中确认。", false, true
	default:
		return "", false, false
	}
}

func (n *Notifier) deliverOnce(ctx context.Context, paneID string, key notificationKey, parts []string) error {
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
		progress := n.pending[paneID]
		if progress == nil || !sameNotificationIdentity(progress.key, key) ||
			(progress.key.kind != "invalidated" && progress.epoch != deliveryEpoch) {
			progress = &notificationProgress{
				key: key, parts: append([]string(nil), parts...), epoch: deliveryEpoch,
			}
			n.pending[paneID] = progress
		} else {
			shared := commonNotificationPrefix(progress.parts, parts)
			if progress.next > shared {
				progress.next = shared
			}
			progress.key = key
			progress.parts = append(progress.parts[:0], parts...)
			progress.epoch = deliveryEpoch
		}
		deliveryParts := progress.parts
		start := progress.next
		n.mu.Unlock()

		var err error
		for index := start; index < len(deliveryParts); index++ {
			if err = n.im.SendMarkdown(ctx, deliveryParts[index]); err != nil {
				break
			}
			n.mu.Lock()
			if n.notificationProgressCurrentLocked(paneID, progress) {
				progress.next = index + 1
			}
			n.mu.Unlock()
		}

		n.mu.Lock()
		if err == nil && n.notificationProgressCurrentLocked(paneID, progress) {
			n.recent[paneID] = progress.key
			delete(n.pending, paneID)
		}
		delete(n.inflight, paneID)
		close(completed)
		n.mu.Unlock()
		return err
	}
}

func (n *Notifier) notificationProgressCurrentLocked(paneID string, progress *notificationProgress) bool {
	if n.pending[paneID] != progress {
		return false
	}
	// invalidation 任务属于 Dispatcher 的 Run 生命周期，必须跨健康 epoch 保留进度。
	return progress.key.kind == "invalidated" || n.epoch == progress.epoch
}

func sameNotificationIdentity(left, right notificationKey) bool {
	return left.paneID == right.paneID &&
		left.occupantKey == right.occupantKey &&
		left.kind == right.kind &&
		left.status == right.status
}

func commonNotificationPrefix(left, right []string) int {
	limit := min(len(left), len(right))
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
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
	content := renderStatusTitle(title, target)
	if len(content) <= panel.WeComContentLimit {
		return []string{content}
	}
	return panel.SplitMarkdown(content, panel.WeComContentLimit)
}

func renderNotificationSnapshot(target session.Target, lines []string) []string {
	content := panel.RenderPage(target, 0, lines)
	content = strings.Replace(content, "第 0 页", "范围：最近最多 100 行", 1)
	return panel.SplitMarkdown(content, panel.WeComContentLimit)
}

func lastNotificationLines(lines []string) []string {
	if len(lines) > panel.PageSize {
		lines = lines[len(lines)-panel.PageSize:]
	}
	return append([]string(nil), lines...)
}

func notificationHash(lines []string) string {
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", digest)
}
