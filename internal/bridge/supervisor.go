package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/session"
)

const (
	defaultSupervisorDebounce      = 100 * time.Millisecond
	defaultSupervisorSnapshot      = 10 * time.Second
	defaultProtocolProbeInterval   = 30 * time.Second
	defaultSupervisorBackoffMin    = time.Second
	defaultSupervisorBackoffMax    = 30 * time.Second
	defaultSupervisorStableWindow  = 30 * time.Second
	supervisorEventChannelCapacity = 64
)

// ErrInvalidSupervisorDependency 表示 Supervisor 缺少必需依赖或依赖关系不一致。
var ErrInvalidSupervisorDependency = errors.New("Supervisor 依赖无效")

// ManagedHerdr 是状态监督所需的 Herdr 公共 API 能力。
type ManagedHerdr interface {
	HerdrAPI
	// CheckCompatible 检查运行中 Herdr 的公开协议版本。
	CheckCompatible(ctx context.Context) error
	// Snapshot 读取完整的当前会话快照。
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	// Subscribe 建立一条独立的公开事件订阅流。
	Subscribe(ctx context.Context, specs []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error)
}

// HerdrFactory 建立一次可供 Supervisor 探测和使用的 Herdr 客户端。
type HerdrFactory interface {
	// Connect 返回一个尚未通过协议兼容性检查的客户端。
	Connect(ctx context.Context) (ManagedHerdr, error)
}

// RetryPolicy 提供可重置的有上限退避，供 Herdr 重连或通知重试使用。
type RetryPolicy interface {
	// Next 返回下一次重试前的等待时间。
	Next() time.Duration
	// Reset 在健康订阅周期建立后重置退避级数。
	Reset()
}

// SupervisorWaitFunc 等待指定时间，并在 context 取消时提前返回。
type SupervisorWaitFunc func(ctx context.Context, delay time.Duration) error

// SupervisorOptions 配置可测试的重连和生命周期 debounce 行为。
type SupervisorOptions struct {
	// Logger 接收状态变化与通知调度诊断日志；nil 时丢弃日志。
	Logger *slog.Logger
	// Backoff 是普通连接、快照或事件流失败后的退避策略。
	Backoff RetryPolicy
	// Wait 同时承载退避和可取消 debounce 等待。
	Wait SupervisorWaitFunc
	// DebounceDelay 是生命周期事件合并窗口。
	DebounceDelay time.Duration
	// SnapshotInterval 是健康周期主动重新读取权威会话快照的间隔。
	SnapshotInterval time.Duration
	// ProtocolProbeInterval 是协议不匹配时的慢探测间隔。
	ProtocolProbeInterval time.Duration
	// StableWindow 是健康周期持续多久后才重置普通退避。
	StableWindow time.Duration
	// Now 返回当前时间，使稳定窗口可在测试中推进。
	Now func() time.Time
	// NotificationQueueCapacity 是待发送主动通知的有界队列容量。
	NotificationQueueCapacity int
	// NotificationBackoff 是主动通知发送失败后的独立退避策略。
	NotificationBackoff RetryPolicy
	// NotificationWait 是主动通知重试使用的可取消等待函数。
	NotificationWait SupervisorWaitFunc
}

// Supervisor 维护 Herdr snapshot、生命周期订阅、状态订阅和重连状态机。
type Supervisor struct {
	factory  HerdrFactory
	registry *session.Registry
	service  *Service
	notifier *Notifier
	logger   *slog.Logger

	backoff                   RetryPolicy
	wait                      SupervisorWaitFunc
	debounceDelay             time.Duration
	snapshotInterval          time.Duration
	protocolProbeInterval     time.Duration
	stableWindow              time.Duration
	now                       func() time.Time
	notificationQueueCapacity int
	notificationBackoff       RetryPolicy
	notificationWait          SupervisorWaitFunc

	// 仅供同包测试观察主循环已取出的消息；nil 时没有运行时行为。
	messageObserved func(supervisorMessage)
}

// NewSupervisor 创建 Herdr 状态监督器并补全默认重连策略。
func NewSupervisor(factory HerdrFactory, registry *session.Registry, service *Service, notifier *Notifier, options SupervisorOptions) (*Supervisor, error) {
	if factory == nil || registry == nil || service == nil || notifier == nil || service.registry != registry {
		return nil, ErrInvalidSupervisorDependency
	}
	if options.Backoff == nil {
		options.Backoff = newDefaultSupervisorRetry(defaultSupervisorBackoffMin, defaultSupervisorBackoffMax, nil)
	}
	if options.Wait == nil {
		options.Wait = waitSupervisorDelay
	}
	if options.DebounceDelay <= 0 {
		options.DebounceDelay = defaultSupervisorDebounce
	}
	if options.SnapshotInterval <= 0 {
		options.SnapshotInterval = defaultSupervisorSnapshot
	}
	if options.ProtocolProbeInterval <= 0 {
		options.ProtocolProbeInterval = defaultProtocolProbeInterval
	}
	if options.StableWindow <= 0 {
		options.StableWindow = defaultSupervisorStableWindow
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NotificationQueueCapacity <= 0 {
		options.NotificationQueueCapacity = defaultNotificationQueueCapacity
	}
	if options.NotificationBackoff == nil {
		options.NotificationBackoff = newDefaultSupervisorRetry(defaultSupervisorBackoffMin, defaultSupervisorBackoffMax, nil)
	}
	if options.NotificationWait == nil {
		options.NotificationWait = waitSupervisorDelay
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{
		factory: factory, registry: registry, service: service, notifier: notifier, logger: options.Logger,
		backoff: options.Backoff, wait: options.Wait,
		debounceDelay: options.DebounceDelay, snapshotInterval: options.SnapshotInterval,
		protocolProbeInterval: options.ProtocolProbeInterval,
		stableWindow:          options.StableWindow, now: options.Now,
		notificationQueueCapacity: options.NotificationQueueCapacity,
		notificationBackoff:       options.NotificationBackoff, notificationWait: options.NotificationWait,
	}, nil
}

// Run 维护 Herdr snapshot、订阅和重连状态机，直到 context 被取消。
func (s *Supervisor) Run(ctx context.Context) error {
	if s == nil {
		return ErrInvalidSupervisorDependency
	}
	parentContext := ctx
	runContext, cancelRun := context.WithCancelCause(ctx)
	dispatcher := newNotificationDispatcher(s.notifier, notificationDispatcherOptions{
		Capacity: s.notificationQueueCapacity,
		Backoff:  s.notificationBackoff,
		Wait:     s.notificationWait,
		Logger:   s.logger,
	})
	dispatcherResult := make(chan error, 1)
	go func() {
		err := dispatcher.Run(runContext)
		if err != nil && !errors.Is(err, context.Canceled) {
			cancelRun(err)
		}
		dispatcherResult <- err
	}()
	defer func() {
		cancelRun(context.Canceled)
		<-dispatcherResult
	}()
	for {
		if err := supervisorRunError(parentContext, runContext, nil); err != nil {
			return err
		}

		client, err := s.factory.Connect(runContext)
		if err != nil {
			s.degrade()
			if runErr := supervisorRunError(parentContext, runContext, nil); runErr != nil {
				return runErr
			}
			if waitErr := s.waitCycleRetry(runContext, err); waitErr != nil {
				return supervisorRunError(parentContext, runContext, waitErr)
			}
			continue
		}
		if err := client.CheckCompatible(runContext); err != nil {
			s.degrade()
			if runErr := supervisorRunError(parentContext, runContext, nil); runErr != nil {
				return runErr
			}
			if waitErr := s.waitCycleRetry(runContext, err); waitErr != nil {
				return supervisorRunError(parentContext, runContext, waitErr)
			}
			continue
		}

		stable, err := s.runHealthyCycle(runContext, client, dispatcher)
		s.degrade()
		if runErr := supervisorRunError(parentContext, runContext, nil); runErr != nil {
			return runErr
		}
		if errors.Is(err, ErrNotificationQueueFull) {
			return err
		}
		if stable {
			s.backoff.Reset()
		}
		if waitErr := s.waitCycleRetry(runContext, err); waitErr != nil {
			return supervisorRunError(parentContext, runContext, waitErr)
		}
	}
}

func supervisorRunError(parentContext, runContext context.Context, fallback error) error {
	if err := parentContext.Err(); err != nil {
		return err
	}
	if runContext.Err() != nil {
		if cause := context.Cause(runContext); cause != nil {
			return cause
		}
		return runContext.Err()
	}
	return fallback
}

func (s *Supervisor) runHealthyCycle(ctx context.Context, client ManagedHerdr, dispatcher *notificationDispatcher) (bool, error) {
	lifecycle, status, statusPlan, baseline, err := s.prepareSubscriptions(ctx, client)
	if err != nil {
		return false, err
	}
	healthyStarted := s.now()
	s.service.ReplaceSnapshot(baseline, true)
	epoch := dispatcher.BeginEpoch()
	s.notifier.Reset()
	s.service.SetHerdr(client)
	defer dispatcher.EndEpoch(epoch)

	cycleContext, cancelCycle := context.WithCancel(ctx)
	messages := make(chan supervisorMessage, supervisorEventChannelCapacity)
	var readers sync.WaitGroup
	startSupervisorReader(cycleContext, &readers, lifecycle, supervisorStreamLifecycle, 0, messages)
	statusGeneration := uint64(0)
	if status != nil {
		statusGeneration++
		startSupervisorReader(cycleContext, &readers, status, supervisorStreamStatus, statusGeneration, messages)
	}
	snapshotTicker := time.NewTicker(s.snapshotInterval)
	defer snapshotTicker.Stop()
	finish := func(err error) (bool, error) {
		return !s.now().Before(healthyStarted.Add(s.stableWindow)), err
	}
	refreshSnapshot := func() error {
		discovery, err := s.snapshot(ctx, client)
		if err != nil {
			return err
		}
		rebuilds := 0
		status, statusPlan, discovery, rebuilds, err = s.reconcileStatusSubscriptions(
			ctx, client, status, statusPlan, discovery,
			func() { statusGeneration++ },
		)
		if err != nil {
			return err
		}
		var changes session.ChangeSet
		if rebuilds == 0 {
			changes = s.service.ReplaceSnapshotPreservingStatus(discovery)
		} else {
			changes = s.service.ReplaceSnapshot(discovery, false)
		}
		for _, target := range changes.RemovedTargets {
			if err := dispatcher.EnqueueInvalidated(target); err != nil {
				return err
			}
		}
		for _, target := range changes.ReplacedTargets {
			if err := dispatcher.EnqueueInvalidated(target); err != nil {
				return err
			}
		}
		if rebuilds > 0 && status != nil {
			startSupervisorReader(cycleContext, &readers, status, supervisorStreamStatus, statusGeneration, messages)
		}
		return nil
	}

	var debounceCancel context.CancelFunc
	defer func() {
		if debounceCancel != nil {
			debounceCancel()
		}
		cancelCycle()
		_ = lifecycle.Close()
		if status != nil {
			_ = status.Close()
		}
		readers.Wait()
	}()

	var debounceGeneration uint64
	for {
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-snapshotTicker.C:
			if err := refreshSnapshot(); err != nil {
				return finish(err)
			}
		case message := <-messages:
			if s.messageObserved != nil {
				s.messageObserved(message)
			}
			switch message.kind {
			case supervisorStreamLifecycle:
				if message.err != nil {
					return finish(message.err)
				}
				if !isLifecycleEvent(message.event.Kind) {
					continue
				}
				debounceGeneration++
				if debounceCancel != nil {
					debounceCancel()
				}
				debounceCancel = scheduleSupervisorDebounce(cycleContext, &readers, s.wait, s.debounceDelay, debounceGeneration, messages)
			case supervisorStreamStatus:
				if message.generation != statusGeneration {
					continue
				}
				if message.err != nil {
					return finish(message.err)
				}
				event, err := herdr.DecodeAgentStatusEvent(message.event)
				if err != nil {
					s.logger.Error("Agent 状态事件解析失败", "error_type", "protocol")
					return finish(err)
				}
				// ApplyStatus 只在 Registry 自身锁内更新状态字段；occupant、选择与 Panel 的
				// 复合变化仍统一由 Service.ReplaceSnapshot 串行处理。
				transition, err := s.registry.ApplyStatus(event)
				if err != nil {
					reason := "registry"
					if errors.Is(err, session.ErrUnknownPane) {
						reason = "unknown_pane"
					} else if errors.Is(err, session.ErrStaleAgentEvent) {
						reason = "stale_agent"
					}
					if reason != "registry" {
						s.logger.Warn("Agent 状态事件已忽略",
							"reason", reason,
							"pane_id", event.PaneID,
							"agent", statusEventAgent(event),
							"current_status", event.AgentStatus,
						)
						continue
					}
					s.logger.Error("Agent 状态事件应用失败",
						"pane_id", event.PaneID,
						"agent", statusEventAgent(event),
						"current_status", event.AgentStatus,
						"error_type", "registry",
					)
					return finish(err)
				}
				if transition.Previous == transition.Current {
					s.logger.Debug("Agent 重复状态事件已忽略", statusTransitionLogArgs(transition)...)
					continue
				}
				_, _, notify := notificationPolicy(transition)
				s.logger.Info("Agent 状态发生变化", append(
					statusTransitionLogArgs(transition), "notification_required", notify,
				)...)
				if err := dispatcher.EnqueueStatus(epoch, transition); err != nil {
					s.logger.Error("Agent 状态通知入队失败", append(
						statusTransitionLogArgs(transition), "error_type", "queue",
					)...)
					return finish(err)
				}
			case supervisorDebounceElapsed:
				if message.generation != debounceGeneration {
					continue
				}
				if debounceCancel != nil {
					debounceCancel()
				}
				debounceCancel = nil
				if err := refreshSnapshot(); err != nil {
					return finish(err)
				}
			}
		}
	}
}

func statusEventAgent(event herdr.AgentStatusEvent) string {
	if event.Agent == nil {
		return ""
	}
	return *event.Agent
}

func statusTransitionLogArgs(transition session.Transition) []any {
	return []any{
		"pane_id", transition.Target.PaneID,
		"occupant_hash", bridgeShortHash(transition.Target.OccupantKey),
		"agent", transition.Target.Agent,
		"previous_status", transition.Previous,
		"current_status", transition.Current,
	}
}

func (s *Supervisor) prepareSubscriptions(ctx context.Context, client ManagedHerdr) (lifecycle herdr.SubscriptionStream, status herdr.SubscriptionStream, statusPlan statusSubscriptionPlan, baseline herdr.Snapshot, err error) {
	discovery, err := s.snapshot(ctx, client)
	if err != nil {
		return nil, nil, statusSubscriptionPlan{}, herdr.Snapshot{}, err
	}
	lifecycle, err = client.Subscribe(ctx, herdr.LifecycleSubscriptions())
	if err != nil {
		return nil, nil, statusSubscriptionPlan{}, herdr.Snapshot{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = lifecycle.Close()
			if status != nil {
				_ = status.Close()
			}
		}
	}()
	statusPlan = statusSubscriptionPlanFromSnapshot(discovery)
	// 专用 status 订阅从创建时的 current_sequence 开始且不重放旧状态；订阅后的
	// 权威 snapshot 用于覆盖 discovery 与订阅确认之间的状态变化。
	status, err = subscribeStatus(ctx, client, statusPlan.specs)
	if err != nil {
		return lifecycle, nil, statusSubscriptionPlan{}, herdr.Snapshot{}, err
	}
	baseline, err = s.snapshot(ctx, client)
	if err != nil {
		return lifecycle, status, statusPlan, herdr.Snapshot{}, err
	}
	status, statusPlan, baseline, _, err = s.reconcileStatusSubscriptions(ctx, client, status, statusPlan, baseline, nil)
	if err != nil {
		return lifecycle, status, statusPlan, herdr.Snapshot{}, err
	}
	cleanup = false
	return lifecycle, status, statusPlan, baseline, nil
}

func (s *Supervisor) reconcileStatusSubscriptions(
	ctx context.Context,
	client ManagedHerdr,
	status herdr.SubscriptionStream,
	statusPlan statusSubscriptionPlan,
	snapshot herdr.Snapshot,
	beforeReplace func(),
) (herdr.SubscriptionStream, statusSubscriptionPlan, herdr.Snapshot, int, error) {
	rebuilds := 0
	for {
		desired := statusSubscriptionPlanFromSnapshot(snapshot)
		if sameStatusSubscriptionPlan(statusPlan, desired) {
			return status, statusPlan, snapshot, rebuilds, nil
		}
		if beforeReplace != nil {
			beforeReplace()
		}
		if status != nil {
			_ = status.Close()
			status = nil
		}
		statusPlan = desired
		var err error
		status, err = subscribeStatus(ctx, client, statusPlan.specs)
		if err != nil {
			return status, statusPlan, herdr.Snapshot{}, rebuilds, err
		}
		rebuilds++
		snapshot, err = s.snapshot(ctx, client)
		if err != nil {
			return status, statusPlan, herdr.Snapshot{}, rebuilds, err
		}
	}
}

func (s *Supervisor) snapshot(ctx context.Context, client ManagedHerdr) (herdr.Snapshot, error) {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return herdr.Snapshot{}, err
	}
	if snapshot.Protocol != herdr.RequiredProtocol {
		return herdr.Snapshot{}, fmt.Errorf("%w: expected %d, got %d", herdr.ErrProtocolMismatch, herdr.RequiredProtocol, snapshot.Protocol)
	}
	return snapshot, nil
}

type statusSubscriptionIdentity struct {
	paneID      string
	occupantKey string
}

type statusSubscriptionPlan struct {
	specs      []herdr.SubscriptionSpec
	identities []statusSubscriptionIdentity
}

func statusSubscriptionPlanFromSnapshot(snapshot herdr.Snapshot) statusSubscriptionPlan {
	registry := &session.Registry{}
	registry.Replace(snapshot, false)
	targets := registry.CreateListSnapshot()
	identities := make([]statusSubscriptionIdentity, 0, len(targets))
	for _, target := range targets {
		if target.PaneID != "" {
			identities = append(identities, statusSubscriptionIdentity{paneID: target.PaneID, occupantKey: target.OccupantKey})
		}
	}
	sort.Slice(identities, func(left, right int) bool { return identities[left].paneID < identities[right].paneID })
	paneIDs := make([]string, len(identities))
	for index, identity := range identities {
		paneIDs[index] = identity.paneID
	}
	return statusSubscriptionPlan{specs: herdr.StatusSubscriptions(paneIDs), identities: identities}
}

func subscribeStatus(ctx context.Context, client ManagedHerdr, specs []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	return client.Subscribe(ctx, specs)
}

func sameStatusSubscriptionPlan(left, right statusSubscriptionPlan) bool {
	if len(left.identities) != len(right.identities) {
		return false
	}
	for index := range left.identities {
		if left.identities[index] != right.identities[index] {
			return false
		}
	}
	return true
}

func (s *Supervisor) degrade() {
	s.service.SetHerdr(nil)
	s.service.InvalidateSelection()
}

func (s *Supervisor) waitRetry(ctx context.Context, delay time.Duration) error {
	if err := s.wait(ctx, delay); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (s *Supervisor) waitCycleRetry(ctx context.Context, cycleErr error) error {
	if errors.Is(cycleErr, herdr.ErrProtocolMismatch) {
		return s.waitRetry(ctx, s.protocolProbeInterval)
	}
	return s.waitRetry(ctx, s.backoff.Next())
}

type supervisorMessageKind uint8

const (
	supervisorStreamLifecycle supervisorMessageKind = iota
	supervisorStreamStatus
	supervisorDebounceElapsed
)

type supervisorMessage struct {
	kind       supervisorMessageKind
	generation uint64
	event      herdr.Event
	err        error
}

func startSupervisorReader(ctx context.Context, group *sync.WaitGroup, stream herdr.SubscriptionStream, kind supervisorMessageKind, generation uint64, messages chan<- supervisorMessage) {
	group.Add(1)
	go func() {
		defer group.Done()
		for {
			event, err := stream.Recv(ctx)
			message := supervisorMessage{kind: kind, generation: generation, event: event, err: err}
			select {
			case messages <- message:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func startSupervisorDebounce(ctx context.Context, group *sync.WaitGroup, wait SupervisorWaitFunc, delay time.Duration, generation uint64, messages chan<- supervisorMessage) {
	group.Add(1)
	go func() {
		defer group.Done()
		if err := wait(ctx, delay); err != nil {
			return
		}
		select {
		case messages <- supervisorMessage{kind: supervisorDebounceElapsed, generation: generation}:
		case <-ctx.Done():
		}
	}()
}

func scheduleSupervisorDebounce(ctx context.Context, group *sync.WaitGroup, wait SupervisorWaitFunc, delay time.Duration, generation uint64, messages chan<- supervisorMessage) context.CancelFunc {
	waitContext, cancel := context.WithCancel(ctx)
	startSupervisorDebounce(waitContext, group, wait, delay, generation, messages)
	return cancel
}

func isLifecycleEvent(kind string) bool {
	switch kind {
	case "pane.created", "pane.closed", "pane.updated", "pane.exited", "pane.agent_detected":
		return true
	default:
		return false
	}
}

type defaultSupervisorRetry struct {
	attempt int
	min     time.Duration
	max     time.Duration
	random  func() float64
}

func newDefaultSupervisorRetry(minimum, maximum time.Duration, random func() float64) *defaultSupervisorRetry {
	if minimum <= 0 {
		minimum = defaultSupervisorBackoffMin
	}
	if maximum < minimum {
		maximum = minimum
	}
	if random == nil {
		random = rand.Float64
	}
	return &defaultSupervisorRetry{min: minimum, max: maximum, random: random}
}

func (r *defaultSupervisorRetry) Next() time.Duration {
	base := r.min
	if r.attempt > 0 {
		shift := min(r.attempt, 62)
		if base > r.max/(1<<shift) {
			base = r.max
		} else {
			base *= 1 << shift
		}
	}
	if base > r.max {
		base = r.max
	}
	r.attempt++
	random := r.random()
	if random < 0 {
		random = 0
	}
	if random > 1 || math.IsNaN(random) {
		random = 1
	}
	delay := time.Duration(float64(base) * (0.8 + random*0.4))
	if delay < r.min {
		return r.min
	}
	if delay > r.max {
		return r.max
	}
	return delay
}

func (r *defaultSupervisorRetry) Reset() {
	r.attempt = 0
}

func waitSupervisorDelay(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
