package dsh

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

const (
	// hostDiscoveryInterval 是多宿主 Backend 重新发现宿主的周期。
	hostDiscoveryInterval = 2 * time.Second
	// backendTargetLimit 与 HPRP 单机快照的目标上限保持一致。
	backendTargetLimit = 256
	// workerShutdownTimeout 限制撤下宿主时等待其组件退出的时间。
	// 单个宿主卡住不能阻塞其他宿主的发现与撤下。
	workerShutdownTimeout = 5 * time.Second
)

// discoverFunc 抽象一次 dsh 宿主发现；测试可替换为固定结果。
type discoverFunc func(context.Context, DiscoveryConfig) ([]Host, error)

// hostFactory 为单个宿主创建连接与执行器；测试可替换为 fake。
type hostFactory func(Host) (*hostWorker, error)

// hostWorker 是一个已发现宿主的运行期资源。
//
// ctx 与 cancel 由 Backend 在加入目录时创建；client 可为 nil（测试替身不需要
// Mux），executor 必须非 nil。两者任一非取消退出都会使整个宿主被撤下，
// 但不会影响其他宿主。
type hostWorker struct {
	host     Host
	ctx      context.Context
	cancel   context.CancelFunc
	client   *Client
	executor *Executor
	done     chan struct{}
}

// Backend 聚合本机多个 dsh 宿主，并实现 relayclient.Executor。
//
// 每 discoveryInterval 重新发现一次宿主：新增宿主启动 Client.Run 与
// Executor.Run，消失宿主立即从目录撤下并取消；某个宿主连接或执行器失败时
// 只撤下该宿主，其他宿主和 Herdr 不受影响。
type Backend struct {
	cfg      DiscoveryConfig
	sink     Sink
	guard    *policy.Guard
	logger   *slog.Logger
	discover discoverFunc
	factory  hostFactory

	// discoveryInterval 在测试中缩短；生产固定 hostDiscoveryInterval。
	discoveryInterval time.Duration

	mu       sync.Mutex
	hosts    map[string]*hostWorker
	selected *hostWorker
}

// NewBackend 创建多宿主 dsh 后端。
//
// 发现使用生产系统探测（discoverHosts + systemProber + 当前 GOOS）；构造时只
// 校验变体配置，不扫描本机、不连接宿主。sink 接收命令回复与状态通知，
// guard 执行用户身份校验。logger 可为 nil。
func NewBackend(cfg DiscoveryConfig, sink Sink, guard *policy.Guard, logger *slog.Logger) (*Backend, error) {
	if err := validateDiscoveryVariant(cfg.Variant); err != nil {
		return nil, err
	}
	if sink == nil {
		return nil, errors.New("dsh Backend 缺少消息 Sink")
	}
	logger = backendLogger(logger)
	discover := func(ctx context.Context, cfg DiscoveryConfig) ([]Host, error) {
		return discoverHosts(ctx, cfg, systemProber{}, runtime.GOOS)
	}
	factory := func(host Host) (*hostWorker, error) {
		client := NewClient(host, logger)
		executor := NewExecutor(host, client, sink, guard)
		executor.logger = logger
		return &hostWorker{host: host, client: client, executor: executor}, nil
	}
	return newBackend(cfg, discover, factory, sink, guard, logger), nil
}

// newBackend 使用注入的发现函数与宿主工厂构造后端，供测试直接模拟多个宿主。
func newBackend(cfg DiscoveryConfig, discover discoverFunc, factory hostFactory, sink Sink, guard *policy.Guard, logger *slog.Logger) *Backend {
	return &Backend{
		cfg:               cfg,
		sink:              sink,
		guard:             guard,
		logger:            backendLogger(logger),
		discover:          discover,
		factory:           factory,
		discoveryInterval: hostDiscoveryInterval,
		hosts:             make(map[string]*hostWorker),
	}
}

// backendLogger 为 nil 时回退到默认 logger。
func backendLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

// Run 持续发现宿主并维护每个宿主的连接与执行器，直到 ctx 结束。
//
// 发现失败保留现有宿主，避免一次系统探测抖动导致全部连接重建；ctx 结束后
// 撤下全部宿主并返回 ctx.Err()。
func (b *Backend) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("dsh Backend 缺少 context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	interval := b.discoveryInterval
	if interval <= 0 {
		interval = hostDiscoveryInterval
	}
	b.syncHosts(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.shutdown()
			return ctx.Err()
		case <-ticker.C:
			b.syncHosts(ctx)
		}
	}
}

// syncHosts 按一次发现结果增删宿主。
//
// discoverFunc 返回错误时不做任何变更；无效宿主与重复引用被跳过。撤下宿主
// 先移出目录再取消，使 CurrentTargets 立即不再暴露失效目标。
func (b *Backend) syncHosts(ctx context.Context) {
	hosts, err := b.discover(ctx, b.cfg)
	if err != nil {
		b.logger.Warn("dsh 宿主发现失败，保留现有连接", "error", err)
		return
	}
	desired := make(map[string]Host, len(hosts))
	for _, host := range hosts {
		if err := host.Validate(); err != nil {
			b.logger.Warn("跳过不可连接的 dsh 宿主", "host", host.Reference(), "error", err)
			continue
		}
		desired[host.Reference()] = host
	}

	b.mu.Lock()
	current := make([]*hostWorker, 0, len(b.hosts))
	for _, worker := range b.hosts {
		current = append(current, worker)
	}
	b.mu.Unlock()

	for _, worker := range current {
		if _, keep := desired[worker.host.Reference()]; !keep {
			b.removeWorker(worker)
		}
	}
	for reference, host := range desired {
		b.mu.Lock()
		_, exists := b.hosts[reference]
		b.mu.Unlock()
		if exists {
			continue
		}
		b.addWorker(ctx, host)
	}
}

// addWorker 为宿主构造执行器并启动其组件。
func (b *Backend) addWorker(ctx context.Context, host Host) {
	worker, err := b.factory(host)
	if err != nil {
		b.logger.Warn("创建 dsh 宿主连接失败", "host", host.Reference(), "error", err)
		return
	}
	if worker == nil {
		b.logger.Warn("创建 dsh 宿主连接失败", "host", host.Reference(), "error", errors.New("宿主工厂返回空 worker"))
		return
	}
	if worker.executor == nil {
		b.logger.Warn("创建 dsh 宿主连接失败", "host", host.Reference(), "error", errors.New("宿主工厂未提供执行器"))
		return
	}
	worker.host = host
	worker.ctx, worker.cancel = context.WithCancel(ctx)
	worker.done = make(chan struct{})

	b.mu.Lock()
	if _, exists := b.hosts[host.Reference()]; exists {
		b.mu.Unlock()
		worker.cancel()
		return
	}
	b.hosts[host.Reference()] = worker
	b.mu.Unlock()
	b.startWorker(worker)
}

// startWorker 并行运行宿主的 Client 与 Executor。
//
// 任一组件以非取消错误退出时立即撤下该宿主并取消另一组件；两个组件都退出后
// 关闭 done，供撤下流程等待。失败直接在组件 goroutine 中更新目录，不依赖
// Run 循环处理，避免通知拥塞时漏掉宿主故障。
func (b *Backend) startWorker(worker *hostWorker) {
	var running sync.WaitGroup
	run := func(component string, fn func(context.Context) error) {
		defer running.Done()
		if err := fn(worker.ctx); err != nil && worker.ctx.Err() == nil {
			b.logger.Warn("dsh 宿主组件退出，撤下该宿主",
				"host", worker.host.Reference(), "component", component, "error", err)
			b.detachWorker(worker)
			if worker.cancel != nil {
				worker.cancel()
			}
		}
	}
	if worker.client != nil {
		running.Add(1)
		go run("client", worker.client.Run)
	}
	running.Add(1)
	go run("executor", worker.executor.Run)
	go func() {
		running.Wait()
		close(worker.done)
	}()
}

// detachWorker 从目录移除宿主并清理选择；重复调用安全。
// 只做目录变更，不取消也不等待组件，以便组件自身退出路径调用。
func (b *Backend) detachWorker(worker *hostWorker) bool {
	if worker == nil {
		return false
	}
	reference := worker.host.Reference()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hosts[reference] != worker {
		return false
	}
	delete(b.hosts, reference)
	if b.selected == worker {
		b.selected = nil
	}
	return true
}

// removeWorker 从目录撤下宿主，取消其组件并等待退出。
//
// 先从目录删除并清理选择，再取消，保证其他读取路径立即看不到该宿主。
// 重复调用安全。
func (b *Backend) removeWorker(worker *hostWorker) {
	if worker == nil {
		return
	}
	reference := worker.host.Reference()
	if !b.detachWorker(worker) {
		return
	}
	if worker.cancel != nil {
		worker.cancel()
	}
	if worker.done == nil {
		return
	}
	select {
	case <-worker.done:
	case <-time.After(workerShutdownTimeout):
		b.logger.Warn("dsh 宿主未在期限内退出", "host", reference)
	}
}

// shutdown 撤下全部宿主并等待其退出。
func (b *Backend) shutdown() {
	b.mu.Lock()
	workers := make([]*hostWorker, 0, len(b.hosts))
	for _, worker := range b.hosts {
		workers = append(workers, worker)
	}
	b.hosts = make(map[string]*hostWorker)
	b.selected = nil
	b.mu.Unlock()

	for _, worker := range workers {
		if worker.cancel != nil {
			worker.cancel()
		}
	}
	for _, worker := range workers {
		if worker.done == nil {
			continue
		}
		select {
		case <-worker.done:
		case <-time.After(workerShutdownTimeout):
			b.logger.Warn("dsh 宿主未在期限内退出", "host", worker.host.Reference())
		}
	}
}

// sortedWorkers 返回按宿主引用稳定排序的目录快照。
func (b *Backend) sortedWorkers() []*hostWorker {
	b.mu.Lock()
	workers := make([]*hostWorker, 0, len(b.hosts))
	for _, worker := range b.hosts {
		workers = append(workers, worker)
	}
	b.mu.Unlock()
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].host.Reference() < workers[j].host.Reference()
	})
	return workers
}

// findWorker 按完整目标定位所属宿主。
func (b *Backend) findWorker(paneID, occupant string) *hostWorker {
	for _, worker := range b.sortedWorkers() {
		if worker.executor == nil {
			continue
		}
		for _, target := range worker.executor.CurrentTargets() {
			if target.PaneID == paneID && target.OccupantKey == occupant {
				return worker
			}
		}
	}
	return nil
}

// CurrentTargets 汇总全部宿主目标，按宿主引用和宿主内既有顺序稳定排序。
//
// 结果最多 backendTargetLimit 条；超出时截断尾部，不跨宿主合并同名会话。
func (b *Backend) CurrentTargets() []session.Target {
	targets := make([]session.Target, 0)
	for _, worker := range b.sortedWorkers() {
		if worker.executor == nil {
			continue
		}
		targets = append(targets, worker.executor.CurrentTargets()...)
	}
	if len(targets) > backendTargetLimit {
		targets = targets[:backendTargetLimit]
	}
	return targets
}

// SelectTarget 按完整目标定位宿主执行器并保留选择。
func (b *Backend) SelectTarget(paneID, occupant string) error {
	worker := b.findWorker(paneID, occupant)
	if worker == nil {
		return session.ErrSelectionInvalid
	}
	if err := worker.executor.SelectTarget(paneID, occupant); err != nil {
		return err
	}
	b.mu.Lock()
	if b.hosts[worker.host.Reference()] != worker {
		b.mu.Unlock()
		return session.ErrSelectionInvalid
	}
	b.selected = worker
	b.mu.Unlock()
	return nil
}

// SelectedTarget 由选中宿主的执行器重新核验当前目标。
func (b *Backend) SelectedTarget() (session.Target, error) {
	worker := b.currentSelection()
	if worker == nil {
		return session.Target{}, session.ErrNoSelection
	}
	return worker.executor.SelectedTarget()
}

// HandleMessage 仅转发给当前选中的宿主执行器。
//
// 选择所属宿主已被撤下时不执行任何动作，由执行器继续执行身份与目标校验。
func (b *Backend) HandleMessage(ctx context.Context, message im.IncomingText) {
	worker := b.currentSelection()
	if worker == nil {
		return
	}
	worker.executor.HandleMessage(ctx, message)
}

// ReadTerminalSnapshot 按完整目标路由到所属宿主执行器，不改变当前选择。
func (b *Backend) ReadTerminalSnapshot(ctx context.Context, paneID, occupant string, mode im.OutputMode, maxLines int) (im.TerminalContent, error) {
	worker := b.findWorker(paneID, occupant)
	if worker == nil {
		return im.TerminalContent{}, session.ErrSelectionInvalid
	}
	return worker.executor.ReadTerminalSnapshot(ctx, paneID, occupant, mode, maxLines)
}

// TakeCreatedTarget 领取最近一次 /new-session 创建的目标。
//
// 优先检查当前选中宿主，再按宿主引用顺序扫描其他宿主；返回并清除第一个找到
// 的目标。不解析消息内容，避免与 relay 的读取顺序耦合。
func (b *Backend) TakeCreatedTarget() (session.Target, bool) {
	selected := b.currentSelection()
	if selected != nil {
		if target, ok := selected.executor.TakeCreatedTarget(); ok {
			return target, true
		}
	}
	for _, worker := range b.sortedWorkers() {
		if worker == selected || worker.executor == nil {
			continue
		}
		if target, ok := worker.executor.TakeCreatedTarget(); ok {
			return target, true
		}
	}
	return session.Target{}, false
}

// currentSelection 返回仍在目录中的选中宿主；宿主被撤下后视为未选择。
func (b *Backend) currentSelection() *hostWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	worker := b.selected
	if worker == nil {
		return nil
	}
	if b.hosts[worker.host.Reference()] != worker {
		return nil
	}
	return worker
}
