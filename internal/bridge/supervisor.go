package bridge

import (
	"context"
	"errors"
	"fmt"
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

// RetryPolicy 提供普通 Herdr 断线后的有上限退避。
type RetryPolicy interface {
	// Next 返回下一次重连前的等待时间。
	Next() time.Duration
	// Reset 在健康订阅周期建立后重置退避级数。
	Reset()
}

// SupervisorWaitFunc 等待指定时间，并在 context 取消时提前返回。
type SupervisorWaitFunc func(ctx context.Context, delay time.Duration) error

// SupervisorOptions 配置可测试的重连和生命周期 debounce 行为。
type SupervisorOptions struct {
	// Backoff 是普通连接、快照或事件流失败后的退避策略。
	Backoff RetryPolicy
	// Wait 同时承载退避和可取消 debounce 等待。
	Wait SupervisorWaitFunc
	// DebounceDelay 是生命周期事件合并窗口。
	DebounceDelay time.Duration
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

	backoff                   RetryPolicy
	wait                      SupervisorWaitFunc
	debounceDelay             time.Duration
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
	return &Supervisor{
		factory: factory, registry: registry, service: service, notifier: notifier,
		backoff: options.Backoff, wait: options.Wait,
		debounceDelay: options.DebounceDelay, protocolProbeInterval: options.ProtocolProbeInterval,
		stableWindow: options.StableWindow, now: options.Now,
		notificationQueueCapacity: options.NotificationQueueCapacity,
		notificationBackoff:       options.NotificationBackoff, notificationWait: options.NotificationWait,
	}, nil
}

// Run 维护 Herdr snapshot、订阅和重连状态机，直到 context 被取消。
func (s *Supervisor) Run(ctx context.Context) error {
	if s == nil {
		return ErrInvalidSupervisorDependency
	}
	dispatcher := newNotificationDispatcher(s.notifier, notificationDispatcherOptions{
		Capacity: s.notificationQueueCapacity,
		Backoff:  s.notificationBackoff,
		Wait:     s.notificationWait,
	})
	dispatcherContext, cancelDispatcher := context.WithCancel(ctx)
	dispatcherResult := make(chan error, 1)
	go func() { dispatcherResult <- dispatcher.Run(dispatcherContext) }()
	defer func() {
		cancelDispatcher()
		<-dispatcherResult
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, err := s.factory.Connect(ctx)
		if err != nil {
			s.degrade()
			if err := s.waitCycleRetry(ctx, err); err != nil {
				return err
			}
			continue
		}
		if err := client.CheckCompatible(ctx); err != nil {
			s.degrade()
			if err := s.waitCycleRetry(ctx, err); err != nil {
				return err
			}
			continue
		}

		stable, err := s.runHealthyCycle(ctx, client, dispatcher)
		s.degrade()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrNotificationQueueFull) {
			return err
		}
		if stable {
			s.backoff.Reset()
		}
		if err := s.waitCycleRetry(ctx, err); err != nil {
			return err
		}
	}
}

func (s *Supervisor) runHealthyCycle(ctx context.Context, client ManagedHerdr, dispatcher *notificationDispatcher) (bool, error) {
	lifecycle, status, statusSpecs, baseline, err := s.prepareSubscriptions(ctx, client)
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
	finish := func(err error) (bool, error) {
		return !s.now().Before(healthyStarted.Add(s.stableWindow)), err
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
					return finish(err)
				}
				// ApplyStatus 只在 Registry 自身锁内更新状态字段；occupant、选择与 Panel 的
				// 复合变化仍统一由 Service.ReplaceSnapshot 串行处理。
				transition, err := s.registry.ApplyStatus(event)
				if err != nil {
					if errors.Is(err, session.ErrUnknownPane) || errors.Is(err, session.ErrStaleAgentEvent) {
						continue
					}
					return finish(err)
				}
				if err := dispatcher.EnqueueStatus(epoch, transition); err != nil {
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
				discovery, err := s.snapshot(ctx, client)
				if err != nil {
					return finish(err)
				}
				rebuilds := 0
				status, statusSpecs, discovery, rebuilds, err = s.reconcileStatusSubscriptions(
					ctx, client, status, statusSpecs, discovery,
					func() { statusGeneration++ },
				)
				if err != nil {
					return finish(err)
				}
				changes := s.service.ReplaceSnapshot(discovery, false)
				for _, target := range changes.RemovedTargets {
					if err := dispatcher.EnqueueInvalidated(target); err != nil {
						return finish(err)
					}
				}
				for _, target := range changes.ReplacedTargets {
					if err := dispatcher.EnqueueInvalidated(target); err != nil {
						return finish(err)
					}
				}
				if rebuilds > 0 && status != nil {
					startSupervisorReader(cycleContext, &readers, status, supervisorStreamStatus, statusGeneration, messages)
				}
			}
		}
	}
}

func (s *Supervisor) prepareSubscriptions(ctx context.Context, client ManagedHerdr) (lifecycle herdr.SubscriptionStream, status herdr.SubscriptionStream, statusSpecs []herdr.SubscriptionSpec, baseline herdr.Snapshot, err error) {
	discovery, err := s.snapshot(ctx, client)
	if err != nil {
		return nil, nil, nil, herdr.Snapshot{}, err
	}
	lifecycle, err = client.Subscribe(ctx, herdr.LifecycleSubscriptions())
	if err != nil {
		return nil, nil, nil, herdr.Snapshot{}, err
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
	statusSpecs = statusSubscriptionsFromSnapshot(discovery)
	status, err = subscribeStatus(ctx, client, statusSpecs)
	if err != nil {
		return lifecycle, nil, nil, herdr.Snapshot{}, err
	}
	baseline, err = s.snapshot(ctx, client)
	if err != nil {
		return lifecycle, status, statusSpecs, herdr.Snapshot{}, err
	}
	status, statusSpecs, baseline, _, err = s.reconcileStatusSubscriptions(ctx, client, status, statusSpecs, baseline, nil)
	if err != nil {
		return lifecycle, status, statusSpecs, herdr.Snapshot{}, err
	}
	cleanup = false
	return lifecycle, status, statusSpecs, baseline, nil
}

func (s *Supervisor) reconcileStatusSubscriptions(
	ctx context.Context,
	client ManagedHerdr,
	status herdr.SubscriptionStream,
	statusSpecs []herdr.SubscriptionSpec,
	snapshot herdr.Snapshot,
	beforeReplace func(),
) (herdr.SubscriptionStream, []herdr.SubscriptionSpec, herdr.Snapshot, int, error) {
	rebuilds := 0
	for {
		desired := statusSubscriptionsFromSnapshot(snapshot)
		if sameSubscriptionSpecs(statusSpecs, desired) {
			return status, statusSpecs, snapshot, rebuilds, nil
		}
		if beforeReplace != nil {
			beforeReplace()
		}
		if status != nil {
			_ = status.Close()
			status = nil
		}
		statusSpecs = desired
		var err error
		status, err = subscribeStatus(ctx, client, statusSpecs)
		if err != nil {
			return status, statusSpecs, herdr.Snapshot{}, rebuilds, err
		}
		rebuilds++
		snapshot, err = s.snapshot(ctx, client)
		if err != nil {
			return status, statusSpecs, herdr.Snapshot{}, rebuilds, err
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

func statusSubscriptionsFromSnapshot(snapshot herdr.Snapshot) []herdr.SubscriptionSpec {
	unique := make(map[string]struct{}, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		if pane.Agent != nil && pane.PaneID != "" {
			unique[pane.PaneID] = struct{}{}
		}
	}
	paneIDs := make([]string, 0, len(unique))
	for paneID := range unique {
		paneIDs = append(paneIDs, paneID)
	}
	sort.Strings(paneIDs)
	return herdr.StatusSubscriptions(paneIDs)
}

func subscribeStatus(ctx context.Context, client ManagedHerdr, specs []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	return client.Subscribe(ctx, specs)
}

func sameSubscriptionSpecs(left, right []herdr.SubscriptionSpec) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
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
