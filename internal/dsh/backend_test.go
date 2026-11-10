package dsh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// backendFakeAPI 是多宿主测试使用的并发安全 API 替身。
type backendFakeAPI struct {
	mu          sync.Mutex
	sessions    []SessionSummary
	pending     PendingSnapshot
	sessionsErr error
	readText    string
	prompts     []fakePromptCall
}

func (f *backendFakeAPI) Sessions(context.Context) ([]SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionSummary(nil), f.sessions...), f.sessionsErr
}

func (f *backendFakeAPI) Pending(context.Context) (PendingSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, nil
}

func (f *backendFakeAPI) Read(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readText, nil
}

func (f *backendFakeAPI) Prompt(_ context.Context, sessionID, requestID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, fakePromptCall{sessionID: sessionID, requestID: requestID, text: text})
	return nil
}

func (f *backendFakeAPI) Create(context.Context, string, string) (CreatedSession, error) {
	return CreatedSession{}, nil
}

func (f *backendFakeAPI) Decisions(context.Context, string) ([]Decision, error) {
	return nil, nil
}

func (f *backendFakeAPI) SubmitAnswer(context.Context, Decision, any) (AnswerReceipt, error) {
	return AnswerReceipt{}, nil
}

func (f *backendFakeAPI) promptCalls() []fakePromptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePromptCall(nil), f.prompts...)
}

func (f *backendFakeAPI) setSessionsErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionsErr = err
}

// backendTestSink 是并发安全的命令回复与通知 sink。
type backendTestSink struct {
	mu      sync.Mutex
	replies []string
}

func (s *backendTestSink) RespondMarkdown(_ context.Context, _ string, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, content)
	return nil
}

func (s *backendTestSink) SendNotification(context.Context, im.NotificationTarget, im.NotificationEvent) error {
	return nil
}

// backendDiscoverer 是可动态修改结果的宿主发现替身。
type backendDiscoverer struct {
	mu    sync.Mutex
	hosts []Host
	err   error
	calls int
}

func (d *backendDiscoverer) discover(context.Context, DiscoveryConfig) ([]Host, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return append([]Host(nil), d.hosts...), d.err
}

func (d *backendDiscoverer) set(hosts []Host, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hosts = append([]Host(nil), hosts...)
	d.err = err
}

func (d *backendDiscoverer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// backendRunner 记录一次 Backend.Run 的退出结果。
type backendRunner struct {
	cancel   context.CancelFunc
	finished chan struct{}
	err      error
}

func startBackendForTest(t *testing.T, backend *Backend) *backendRunner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runner := &backendRunner{cancel: cancel, finished: make(chan struct{})}
	go func() {
		runner.err = backend.Run(ctx)
		close(runner.finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runner.finished:
		case <-time.After(3 * time.Second):
			t.Error("Backend.Run 未在期限内退出")
		}
	})
	return runner
}

// stop 取消 Run 并返回退出错误。
func (r *backendRunner) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case <-r.finished:
		return r.err
	case <-time.After(3 * time.Second):
		t.Fatal("Backend.Run 未在期限内退出")
		return nil
	}
}

func backendTestHost(pid uint32, port uint16, generation string) Host {
	return Host{
		PID:        pid,
		Port:       port,
		Version:    "0.2.1-rc.5",
		Variant:    variantWeb,
		Home:       "/tmp/dsh-backend-test",
		Generation: generation,
	}
}

func backendTestSession(id, title string) SessionSummary {
	return SessionSummary{ID: id, Title: title, Cwd: "/tmp/" + id, Available: true}
}

func backendTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newBackendForTest(t *testing.T, discover discoverFunc, factory hostFactory) *Backend {
	t.Helper()
	guard, err := policy.NewGuard("user")
	if err != nil {
		t.Fatalf("NewGuard() 返回错误：%v", err)
	}
	backend := newBackend(DiscoveryConfig{}, discover, factory, &backendTestSink{}, guard, backendTestLogger())
	backend.discoveryInterval = 10 * time.Millisecond
	return backend
}

// backendWorkerFactory 按宿主引用返回共享 fake API 的 worker。
func backendWorkerFactory(t *testing.T, apis map[string]*backendFakeAPI) hostFactory {
	t.Helper()
	guard, err := policy.NewGuard("user")
	if err != nil {
		t.Fatalf("NewGuard() 返回错误：%v", err)
	}
	logger := backendTestLogger()
	sink := &backendTestSink{}
	return func(host Host) (*hostWorker, error) {
		api, ok := apis[host.Reference()]
		if !ok {
			return nil, fmt.Errorf("测试缺少宿主 %s 的 fake API", host.Reference())
		}
		executor := NewExecutor(host, api, sink, guard)
		executor.logger = logger
		return &hostWorker{host: host, executor: executor}, nil
	}
}

func waitBackend(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待%s超时", description)
}

func targetWithTitle(targets []session.Target, title string) (session.Target, bool) {
	for _, target := range targets {
		if target.Title == title {
			return target, true
		}
	}
	return session.Target{}, false
}

func backendWorkerByRef(t *testing.T, backend *Backend, reference string) *hostWorker {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	worker := backend.hosts[reference]
	if worker == nil {
		t.Fatalf("后端中没有宿主 %s", reference)
	}
	return worker
}

func seedCreatedTarget(t *testing.T, executor *Executor, target session.Target) {
	t.Helper()
	executor.mu.Lock()
	executor.createdTarget = target
	executor.mu.Unlock()
}

func TestBackendAddsAndRemovesHosts(t *testing.T) {
	hostA := backendTestHost(101, 4101, "gen-a")
	hostB := backendTestHost(202, 4102, "gen-b")
	apis := map[string]*backendFakeAPI{
		hostA.Reference(): {sessions: []SessionSummary{backendTestSession("session-a", "Alpha")}},
		hostB.Reference(): {sessions: []SessionSummary{backendTestSession("session-b", "Beta")}},
	}
	discoverer := &backendDiscoverer{hosts: []Host{hostA}}
	backend := newBackendForTest(t, discoverer.discover, backendWorkerFactory(t, apis))
	runner := startBackendForTest(t, backend)

	waitBackend(t, "宿主 A 目标出现", func() bool {
		_, ok := targetWithTitle(backend.CurrentTargets(), "Alpha")
		return ok
	})

	discoverer.set([]Host{hostB, hostA}, nil)
	waitBackend(t, "宿主 B 目标出现", func() bool {
		_, ok := targetWithTitle(backend.CurrentTargets(), "Beta")
		return ok
	})

	// 汇总结果按宿主引用稳定排序，与发现返回顺序无关。
	targets := backend.CurrentTargets()
	indexA, indexB := -1, -1
	for index, target := range targets {
		switch target.Title {
		case "Alpha":
			indexA = index
		case "Beta":
			indexB = index
		}
	}
	if indexA < 0 || indexB < 0 || indexA > indexB {
		t.Fatalf("目标未按宿主引用稳定排序：%#v", targets)
	}

	discoverer.set([]Host{hostB}, nil)
	waitBackend(t, "宿主 A 目标撤下", func() bool {
		_, ok := targetWithTitle(backend.CurrentTargets(), "Alpha")
		return !ok
	})
	if _, ok := targetWithTitle(backend.CurrentTargets(), "Beta"); !ok {
		t.Fatal("宿主 A 消失后宿主 B 目标必须保留")
	}

	if err := runner.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if targets := backend.CurrentTargets(); len(targets) != 0 {
		t.Fatalf("Run 退出后仍有目标：%#v", targets)
	}
}

func TestBackendRoutesSelectionAndMessagesByTarget(t *testing.T) {
	hostA := backendTestHost(111, 4111, "route-a")
	hostB := backendTestHost(222, 4122, "route-b")
	apiA := &backendFakeAPI{sessions: []SessionSummary{backendTestSession("session-a", "Alpha")}, readText: "A transcript"}
	apiB := &backendFakeAPI{sessions: []SessionSummary{backendTestSession("session-b", "Beta")}, readText: "B transcript"}
	apis := map[string]*backendFakeAPI{
		hostA.Reference(): apiA,
		hostB.Reference(): apiB,
	}
	discoverer := &backendDiscoverer{hosts: []Host{hostA, hostB}}
	backend := newBackendForTest(t, discoverer.discover, backendWorkerFactory(t, apis))
	_ = startBackendForTest(t, backend)

	var targetA, targetB session.Target
	waitBackend(t, "两个宿主目标出现", func() bool {
		targets := backend.CurrentTargets()
		foundA, okA := targetWithTitle(targets, "Alpha")
		foundB, okB := targetWithTitle(targets, "Beta")
		targetA, targetB = foundA, foundB
		return okA && okB
	})

	if _, err := backend.SelectedTarget(); !errors.Is(err, session.ErrNoSelection) {
		t.Fatalf("初始 SelectedTarget() 错误 = %v, want ErrNoSelection", err)
	}
	if err := backend.SelectTarget(targetB.PaneID, targetB.OccupantKey); err != nil {
		t.Fatalf("SelectTarget(B) 返回错误：%v", err)
	}
	selected, err := backend.SelectedTarget()
	if err != nil || selected.PaneID != targetB.PaneID || selected.OccupantKey != targetB.OccupantKey {
		t.Fatalf("SelectedTarget() = (%#v, %v), want B", selected, err)
	}

	backend.HandleMessage(context.Background(), im.IncomingText{
		RequestID: "req-1", MessageID: "msg-1", UserID: "user", ChatType: "single", Content: "发给 B",
	})
	callsB := apiB.promptCalls()
	if len(callsB) != 1 || callsB[0].sessionID != "session-b" || callsB[0].text != "发给 B" {
		t.Fatalf("宿主 B prompt 调用 = %#v", callsB)
	}
	if callsA := apiA.promptCalls(); len(callsA) != 0 {
		t.Fatalf("消息被路由到宿主 A：%#v", callsA)
	}

	content, err := backend.ReadTerminalSnapshot(context.Background(), targetB.PaneID, targetB.OccupantKey, im.OutputModeText, 100)
	if err != nil || content.Text != "B transcript" {
		t.Fatalf("ReadTerminalSnapshot(B) = (%#v, %v)", content, err)
	}
	if callsA := apiA.promptCalls(); len(callsA) != 0 {
		t.Fatalf("读取不应路由到宿主 A：%#v", callsA)
	}

	if err := backend.SelectTarget(targetA.PaneID, targetA.OccupantKey); err != nil {
		t.Fatalf("SelectTarget(A) 返回错误：%v", err)
	}
	backend.HandleMessage(context.Background(), im.IncomingText{
		RequestID: "req-2", MessageID: "msg-2", UserID: "user", ChatType: "single", Content: "发给 A",
	})
	callsA := apiA.promptCalls()
	if len(callsA) != 1 || callsA[0].sessionID != "session-a" {
		t.Fatalf("宿主 A prompt 调用 = %#v", callsA)
	}
	if callsB := apiB.promptCalls(); len(callsB) != 1 {
		t.Fatalf("切换选择后宿主 B 收到额外消息：%#v", callsB)
	}

	if err := backend.SelectTarget("missing", "stale"); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("SelectTarget(失效目标) = %v, want ErrSelectionInvalid", err)
	}
	if _, err := backend.ReadTerminalSnapshot(context.Background(), "missing", "stale", im.OutputModeText, 10); !errors.Is(err, session.ErrSelectionInvalid) {
		t.Fatalf("ReadTerminalSnapshot(失效目标) = %v, want ErrSelectionInvalid", err)
	}
}

func TestBackendKeepsOtherHostsWhenOneFails(t *testing.T) {
	badHost := backendTestHost(301, 4301, "broken")
	goodHost := backendTestHost(302, 4302, "healthy")
	apis := map[string]*backendFakeAPI{
		badHost.Reference():  {sessions: []SessionSummary{backendTestSession("bad-session", "Bad")}},
		goodHost.Reference(): {sessions: []SessionSummary{backendTestSession("good-session", "Good")}},
	}
	discoverer := &backendDiscoverer{hosts: []Host{badHost, goodHost}}

	guard, err := policy.NewGuard("user")
	if err != nil {
		t.Fatalf("NewGuard() 返回错误：%v", err)
	}
	logger := backendTestLogger()
	sink := &backendTestSink{}
	// 相对 Home 让 Mux 在启动时命中不可重试配置错误，模拟单个宿主整体失效。
	brokenHost := badHost
	brokenHost.Home = "relative-home"
	var badFactoryCalls atomic.Int64
	factory := func(host Host) (*hostWorker, error) {
		executor := NewExecutor(host, apis[host.Reference()], sink, guard)
		executor.logger = logger
		worker := &hostWorker{host: host, executor: executor}
		if host.Reference() == badHost.Reference() {
			badFactoryCalls.Add(1)
			worker.client = NewClient(brokenHost, logger)
		}
		return worker, nil
	}
	backend := newBackend(DiscoveryConfig{}, discoverer.discover, factory, sink, guard, logger)
	backend.discoveryInterval = 10 * time.Millisecond
	runner := startBackendForTest(t, backend)

	waitBackend(t, "故障宿主被创建", func() bool { return badFactoryCalls.Load() > 0 })
	// 停止继续发现故障宿主，避免测试期间反复重建。
	discoverer.set([]Host{goodHost}, nil)

	waitBackend(t, "故障宿主撤下且健康宿主保留", func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		_, badPresent := backend.hosts[badHost.Reference()]
		_, goodPresent := backend.hosts[goodHost.Reference()]
		return !badPresent && goodPresent
	})
	waitBackend(t, "健康宿主目标保留", func() bool {
		_, ok := targetWithTitle(backend.CurrentTargets(), "Good")
		return ok
	})

	var goodTarget session.Target
	waitBackend(t, "健康宿主目标可获取", func() bool {
		target, ok := targetWithTitle(backend.CurrentTargets(), "Good")
		goodTarget = target
		return ok
	})
	if err := backend.SelectTarget(goodTarget.PaneID, goodTarget.OccupantKey); err != nil {
		t.Fatalf("健康宿主不可选择：%v", err)
	}
	backend.HandleMessage(context.Background(), im.IncomingText{
		RequestID: "req-good", MessageID: "msg-good", UserID: "user", ChatType: "single", Content: "继续工作",
	})
	if calls := apis[goodHost.Reference()].promptCalls(); len(calls) != 1 || calls[0].sessionID != "good-session" {
		t.Fatalf("健康宿主 prompt 调用 = %#v", calls)
	}

	// 一次发现失败不应拆掉已连接的宿主，等待多个发现周期后目标仍然存在。
	callsBefore := discoverer.callCount()
	discoverer.set(nil, errors.New("ps 探测失败"))
	waitBackend(t, "重新发现发生", func() bool { return discoverer.callCount() >= callsBefore+3 })
	if _, ok := targetWithTitle(backend.CurrentTargets(), "Good"); !ok {
		t.Fatal("发现失败后健康宿主目标被错误撤下")
	}
	select {
	case <-runner.finished:
		t.Fatal("单个宿主失败或发现错误导致 Backend.Run 退出")
	default:
	}
}

func TestBackendTakeCreatedTargetDelegatesToSelectedHost(t *testing.T) {
	hostA := backendTestHost(401, 4401, "created-a")
	hostB := backendTestHost(402, 4402, "created-b")
	apis := map[string]*backendFakeAPI{
		hostA.Reference(): {sessions: []SessionSummary{backendTestSession("session-a", "Alpha")}},
		hostB.Reference(): {sessions: []SessionSummary{backendTestSession("session-b", "Beta")}},
	}
	discoverer := &backendDiscoverer{hosts: []Host{hostA, hostB}}
	backend := newBackendForTest(t, discoverer.discover, backendWorkerFactory(t, apis))
	_ = startBackendForTest(t, backend)

	var targetB session.Target
	waitBackend(t, "两个宿主目标出现", func() bool {
		targets := backend.CurrentTargets()
		found, ok := targetWithTitle(targets, "Beta")
		targetB = found
		if !ok {
			return false
		}
		_, otherOK := targetWithTitle(targets, "Alpha")
		return otherOK
	})
	if err := backend.SelectTarget(targetB.PaneID, targetB.OccupantKey); err != nil {
		t.Fatalf("SelectTarget(B) 返回错误：%v", err)
	}

	workerB := backendWorkerByRef(t, backend, hostB.Reference())
	createdB := session.Target{PaneID: "created-by-b", OccupantKey: "occupant-b", Agent: dshAgent}
	seedCreatedTarget(t, workerB.executor, createdB)

	taken, ok := backend.TakeCreatedTarget()
	if !ok || taken.PaneID != createdB.PaneID || taken.OccupantKey != createdB.OccupantKey {
		t.Fatalf("TakeCreatedTarget() = (%#v, %v), want %#v", taken, ok, createdB)
	}
	if _, ok := backend.TakeCreatedTarget(); ok {
		t.Fatal("TakeCreatedTarget() 第二次调用必须返回 false")
	}

	workerA := backendWorkerByRef(t, backend, hostA.Reference())
	createdA := session.Target{PaneID: "created-by-a", OccupantKey: "occupant-a", Agent: dshAgent}
	seedCreatedTarget(t, workerA.executor, createdA)
	taken, ok = backend.TakeCreatedTarget()
	if !ok || taken.PaneID != createdA.PaneID {
		t.Fatalf("选中宿主没有目标时应回退扫描其他宿主，得到 (%#v, %v)", taken, ok)
	}
}

func TestBackendLimitsAggregatedTargets(t *testing.T) {
	host := backendTestHost(501, 4501, "limit")
	sessions := make([]SessionSummary, 0, backendTargetLimit+44)
	for index := 0; index < backendTargetLimit+44; index++ {
		sessions = append(sessions, backendTestSession(fmt.Sprintf("session-%03d", index), fmt.Sprintf("Session %03d", index)))
	}
	apis := map[string]*backendFakeAPI{host.Reference(): {sessions: sessions}}
	discoverer := &backendDiscoverer{hosts: []Host{host}}
	backend := newBackendForTest(t, discoverer.discover, backendWorkerFactory(t, apis))
	_ = startBackendForTest(t, backend)

	waitBackend(t, "超量宿主目标出现", func() bool { return len(backend.CurrentTargets()) > 0 })
	if got := len(backend.CurrentTargets()); got != backendTargetLimit {
		t.Fatalf("CurrentTargets() 数量 = %d, want %d", got, backendTargetLimit)
	}
}
