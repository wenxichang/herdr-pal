package bridge

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
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
}

// Supervisor 维护 Herdr snapshot、生命周期订阅、状态订阅和重连状态机。
type Supervisor struct {
	factory  HerdrFactory
	registry *session.Registry
	service  *Service
	notifier *Notifier

	backoff               RetryPolicy
	wait                  SupervisorWaitFunc
	debounceDelay         time.Duration
	protocolProbeInterval time.Duration
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
	return &Supervisor{
		factory: factory, registry: registry, service: service, notifier: notifier,
		backoff: options.Backoff, wait: options.Wait,
		debounceDelay: options.DebounceDelay, protocolProbeInterval: options.ProtocolProbeInterval,
	}, nil
}

// Run 维护 Herdr snapshot、订阅和重连状态机，直到 context 被取消。
func (s *Supervisor) Run(ctx context.Context) error {
	if s == nil {
		return ErrInvalidSupervisorDependency
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, err := s.factory.Connect(ctx)
		if err != nil {
			s.degrade()
			if err := s.waitRetry(ctx, s.backoff.Next()); err != nil {
				return err
			}
			continue
		}
		if err := client.CheckCompatible(ctx); err != nil {
			s.degrade()
			var delay time.Duration
			if errors.Is(err, herdr.ErrProtocolMismatch) {
				delay = s.protocolProbeInterval
			} else {
				delay = s.backoff.Next()
			}
			if err := s.waitRetry(ctx, delay); err != nil {
				return err
			}
			continue
		}
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			s.degrade()
			if err := s.waitRetry(ctx, s.backoff.Next()); err != nil {
				return err
			}
			continue
		}

		s.service.ReplaceSnapshot(snapshot, true)
		s.notifier.Reset()
		s.service.SetHerdr(client)
		err = s.runHealthyCycle(ctx, client)
		s.degrade()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.waitRetry(ctx, s.backoff.Next()); err != nil {
			return err
		}
	}
}

func (s *Supervisor) runHealthyCycle(ctx context.Context, client ManagedHerdr) error {
	lifecycle, err := client.Subscribe(ctx, herdr.LifecycleSubscriptions())
	if err != nil {
		return err
	}
	statusSpecs := herdr.StatusSubscriptions(s.registry.AgentPaneIDs())
	var status herdr.SubscriptionStream
	if len(statusSpecs) > 0 {
		status, err = client.Subscribe(ctx, statusSpecs)
		if err != nil {
			_ = lifecycle.Close()
			return err
		}
	}

	cycleContext, cancelCycle := context.WithCancel(ctx)
	messages := make(chan supervisorMessage, supervisorEventChannelCapacity)
	var readers sync.WaitGroup
	startSupervisorReader(cycleContext, &readers, lifecycle, supervisorStreamLifecycle, 0, messages)
	statusGeneration := uint64(0)
	if status != nil {
		statusGeneration++
		startSupervisorReader(cycleContext, &readers, status, supervisorStreamStatus, statusGeneration, messages)
	}
	s.backoff.Reset()

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
			return ctx.Err()
		case message := <-messages:
			switch message.kind {
			case supervisorStreamLifecycle:
				if message.err != nil {
					return message.err
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
					return message.err
				}
				event, err := herdr.DecodeAgentStatusEvent(message.event)
				if err != nil {
					return err
				}
				// ApplyStatus 只在 Registry 自身锁内更新状态字段；occupant、选择与 Panel 的
				// 复合变化仍统一由 Service.ReplaceSnapshot 串行处理。
				transition, err := s.registry.ApplyStatus(event)
				if err != nil {
					if errors.Is(err, session.ErrUnknownPane) || errors.Is(err, session.ErrStaleAgentEvent) {
						continue
					}
					return err
				}
				_ = s.notifier.HandleTransition(ctx, transition)
			case supervisorDebounceElapsed:
				if message.generation != debounceGeneration {
					continue
				}
				if debounceCancel != nil {
					debounceCancel()
				}
				debounceCancel = nil
				snapshot, err := client.Snapshot(ctx)
				if err != nil {
					return err
				}
				changes := s.service.ReplaceSnapshot(snapshot, false)
				for _, target := range changes.RemovedTargets {
					_ = s.notifier.TargetInvalidated(ctx, target)
				}
				for _, target := range changes.ReplacedTargets {
					_ = s.notifier.TargetInvalidated(ctx, target)
				}
				if !changes.AgentSetChanged {
					continue
				}

				statusGeneration++
				if status != nil {
					_ = status.Close()
					status = nil
				}
				statusSpecs = herdr.StatusSubscriptions(s.registry.AgentPaneIDs())
				if len(statusSpecs) == 0 {
					continue
				}
				status, err = client.Subscribe(ctx, statusSpecs)
				if err != nil {
					return err
				}
				startSupervisorReader(cycleContext, &readers, status, supervisorStreamStatus, statusGeneration, messages)
			}
		}
	}
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
