package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestRunRejectsConfigurationBeforeStartingConnections(t *testing.T) {
	connectionCalls := 0
	options := testOptions(t)
	options.dependencies.loadConfig = func(string, func(string) string) (config.Config, error) {
		return config.Config{}, errors.New("配置损坏")
	}
	options.dependencies.acquireLock = func(string) (processLock, error) {
		connectionCalls++
		return &fakeLock{}, nil
	}
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		connectionCalls++
		return "", nil
	}
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		connectionCalls++
		return nil, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "配置损坏") {
		t.Fatalf("Run() error = %v, want wrapped configuration error", err)
	}
	if connectionCalls != 0 {
		t.Fatalf("connection setup calls = %d, want 0", connectionCalls)
	}
}

func TestRunReportsLockConflictWithoutResolvingSocket(t *testing.T) {
	options := testOptions(t)
	resolveCalls := 0
	options.dependencies.acquireLock = func(string) (processLock, error) {
		return nil, processlock.ErrAlreadyRunning
	}
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		resolveCalls++
		return "", nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, processlock.ErrAlreadyRunning) || !strings.Contains(err.Error(), "已在运行") {
		t.Fatalf("Run() error = %v, want clear lock conflict", err)
	}
	if resolveCalls != 0 {
		t.Fatalf("ResolveSocket calls = %d, want 0", resolveCalls)
	}
}

func TestRunResolvesSocketBeforeAssemblyAndUsesSafeLockName(t *testing.T) {
	options := testOptions(t)
	var lockPath, assembledSocket string
	options.dependencies.acquireLock = func(path string) (processLock, error) {
		lockPath = path
		return &fakeLock{}, nil
	}
	options.dependencies.resolveSocket = func(_ context.Context, explicit, sessionName string, runner herdr.CommandRunner) (string, error) {
		if explicit != "" || sessionName != "named-session" || runner == nil {
			t.Fatalf("ResolveSocket args = %q, %q, %v", explicit, sessionName, runner)
		}
		return "/tmp/herdr-test.sock", nil
	}
	options.dependencies.assemble = func(_ config.Config, socket string, _ *slog.Logger) (*applicationRuntime, error) {
		assembledSocket = socket
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if assembledSocket != "/tmp/herdr-test.sock" {
		t.Fatalf("assembled socket = %q", assembledSocket)
	}
	if !strings.HasPrefix(lockPath, "/cache/herdr-pal/") || !strings.HasSuffix(lockPath, ".lock") {
		t.Fatalf("lock path = %q", lockPath)
	}
	for _, sensitive := range []string{"bot-sensitive", "secret-sensitive"} {
		if strings.Contains(lockPath, sensitive) {
			t.Fatalf("lock path contains sensitive value %q: %q", sensitive, lockPath)
		}
	}
}

func TestAssembleRuntimeSharesOneHerdrClientAcrossAllBridgeUsers(t *testing.T) {
	managed := newFakeManagedHerdr()
	im := newFakeWeCom()
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(socketPath string) bridge.ManagedHerdr {
		if socketPath != "/tmp/shared.sock" {
			t.Fatalf("newHerdr socket = %q", socketPath)
		}
		return managed
	}
	dependencies.newWeCom = func(clientConfig wecom.ClientConfig) (weComRuntime, error) {
		if clientConfig.Endpoint != wecom.DefaultEndpoint || clientConfig.BotID != "bot-sensitive" ||
			clientConfig.AllowedUserID != "user-sensitive" || clientConfig.Secret != "secret-sensitive" {
			t.Fatalf("WeCom config = %s", clientConfig)
		}
		return im, nil
	}

	runtime, err := assembleRuntime(testConfig(), "/tmp/shared.sock", slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies)
	if err != nil {
		t.Fatalf("assembleRuntime() error = %v", err)
	}
	connected, err := runtime.factory.Connect(context.Background())
	if err != nil || connected != managed {
		t.Fatalf("factory.Connect() = %v, %v, want shared client", connected, err)
	}

	runtime.service.SetHerdr(managed)
	runtime.service.ReplaceSnapshot(managed.snapshot, false)
	for index, content := range []string{"/ls", "/sel 1", "继续处理"} {
		runtime.service.HandleMessage(context.Background(), incoming("service-"+string(rune('a'+index)), content))
	}
	if got := managed.promptCount(); got != 1 {
		t.Fatalf("shared client prompt calls = %d, want 1", got)
	}
	getsBeforeNotification := managed.getCount()

	target := (&session.Registry{})
	target.Replace(managed.snapshot, false)
	selected := target.CreateListSnapshot()[0]
	err = runtime.notifier.HandleTransition(context.Background(), session.Transition{
		Target: selected, Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusBlocked,
	})
	if err != nil {
		t.Fatalf("Notifier.HandleTransition() error = %v", err)
	}
	if gotGet, gotRead := managed.getCount()-getsBeforeNotification, managed.readCount(); gotGet != 2 || gotRead != 1 {
		t.Fatalf("shared notifier get/read calls = %d/%d, want 2/1", gotGet, gotRead)
	}
}

func TestAssembleRuntimeUsesOfficialWeComEndpointByDefault(t *testing.T) {
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(string) bridge.ManagedHerdr { return newFakeManagedHerdr() }
	dependencies.newWeCom = func(clientConfig wecom.ClientConfig) (weComRuntime, error) {
		if clientConfig.Endpoint != wecom.DefaultEndpoint {
			t.Fatalf("WeCom endpoint = %q, want %q", clientConfig.Endpoint, wecom.DefaultEndpoint)
		}
		return newFakeWeCom(), nil
	}

	if _, err := assembleRuntime(testConfig(), "/tmp/shared.sock", slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies); err != nil {
		t.Fatalf("assembleRuntime() error = %v", err)
	}
}

func TestRunStartsAllLoopsAndConsumesMessages(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	handler := &fakeHandler{handled: make(chan wecom.IncomingText, 1)}
	runtime := &applicationRuntime{wecom: im, supervisor: supervisor, handler: handler}
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) { return runtime, nil }

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "WeCom Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	im.events <- incoming("message-1", "/ls")
	select {
	case message := <-handler.handled:
		if message.MessageID != "message-1" {
			t.Fatalf("handled message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("message consumer did not start")
	}
	cancel()
	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunDoesNotStartLoopsWhenContextIsAlreadyCanceled(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for name, started := range map[string]<-chan struct{}{"WeCom": im.started, "Supervisor": supervisor.started} {
		select {
		case <-started:
			t.Fatalf("%s loop started with an already canceled context", name)
		default:
		}
	}
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 without live components", got)
	}
}

func TestRunCancelsOtherLoopsAfterFatalError(t *testing.T) {
	fatal := errors.New("supervisor fatal")
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	supervisor.result = fatal
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want fatal supervisor error", err)
	}
	waitClosed(t, im.stopped, "WeCom cancellation")
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 after fatal shutdown", got)
	}
}

func TestRunPreservesWeComFatalRegardlessOfResultOrder(t *testing.T) {
	fatal := errors.New("wecom fatal")
	tests := []struct {
		name        string
		closeEvents bool
		waitForStop bool
	}{
		{name: "WeCom 错误先到"},
		{name: "Events 关闭先到", closeEvents: true, waitForStop: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newFakeRunner()
			im := newOrderedResultWeCom(fatal)
			im.closeEvents = test.closeEvents
			if test.waitForStop {
				im.returnAfter = supervisor.stopped
			}
			options := testOptions(t)
			options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
				return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
			}

			err := Run(context.Background(), options)
			if !errors.Is(err, fatal) {
				t.Fatalf("Run() error = %v, want WeCom fatal", err)
			}
			waitClosed(t, supervisor.stopped, "Supervisor cancellation")
		})
	}
}

func TestRunKeepsFatalThatTriggeredShutdownWhenParentCancelsDuringDrain(t *testing.T) {
	fatal := errors.New("wecom fatal")
	releaseSupervisor := make(chan struct{})
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(fatal)
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, supervisor.canceled, "fatal-triggered Supervisor cancellation")
	cancel()
	close(releaseSupervisor)

	err := waitResult(t, result)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want original fatal after late parent cancellation", err)
	}
}

func TestRunTimeoutKeepsFatalThatTriggeredShutdownBeforeParentCancellation(t *testing.T) {
	fatal := errors.New("wecom fatal")
	releaseSupervisor := make(chan struct{})
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(fatal)
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, supervisor.canceled, "fatal-triggered Supervisor cancellation")
	cancel()

	err := waitResult(t, result)
	if !errors.Is(err, fatal) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want fatal joined with ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 {
		t.Fatalf("lock releases = %d, want 0 while Supervisor remains blocked", got)
	}
	close(releaseSupervisor)
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunParentCancellationFirstRemainsNormal(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "WeCom Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil when parent cancellation triggered shutdown", err)
	}
}

func TestRunParentCancellationWithWeComClosingEventsRemainsNormal(t *testing.T) {
	for iteration := range 50 {
		im := newCancelClosingWeCom()
		supervisor := newFakeRunner()
		handler := &fakeHandler{handled: make(chan wecom.IncomingText, 1)}
		options := testOptions(t)
		options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
			return &applicationRuntime{wecom: im, supervisor: supervisor, handler: handler}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- Run(ctx, options) }()
		waitClosed(t, im.started, "WeCom Run")
		waitClosed(t, supervisor.started, "Supervisor Run")
		im.events <- incoming(fmt.Sprintf("warmup-%d", iteration), "/ls")
		select {
		case <-handler.handled:
		case <-time.After(time.Second):
			t.Fatal("message consumer did not process warmup event")
		}
		cancel()
		if err := waitResult(t, result); err != nil {
			t.Fatalf("iteration %d: Run() error = %v, want nil", iteration, err)
		}
	}
}

func TestConsumeSelectedMessagePrefersCancellationOverClosedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done, err := consumeSelectedMessage(ctx, &fakeHandler{}, wecom.IncomingText{}, false)
	if !done || !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeSelectedMessage() = %v, %v, want done with context.Canceled", done, err)
	}

	done, err = consumeSelectedMessage(context.Background(), &fakeHandler{}, wecom.IncomingText{}, false)
	if !done || err != nil {
		t.Fatalf("active closed Events = %v, %v, want clean unexpected closure", done, err)
	}
}

func TestParentTriggeredShutdownClassifiesSimultaneousResultDeterministically(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name           string
		selectedParent bool
		result         *componentResult
		want           bool
	}{
		{name: "直接选中 parent", selectedParent: true, want: true},
		{name: "选中 parent 派生取消", result: &componentResult{err: context.Canceled, shutdownDerived: true}, want: true},
		{name: "选中同时到达的真实错误", result: &componentResult{err: errors.New("fatal")}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parentTriggeredShutdown(ctx, test.selectedParent, test.result); got != test.want {
				t.Fatalf("parentTriggeredShutdown() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRunTreatsIndependentEventsClosureAsUnexpectedLoopStop(t *testing.T) {
	supervisor := newFakeRunner()
	im := newOrderedResultWeCom(nil)
	im.closeEvents = true
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, ErrLoopStopped) {
		t.Fatalf("Run() error = %v, want ErrLoopStopped", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, cancellation must not replace ErrLoopStopped", err)
	}
	waitClosed(t, supervisor.stopped, "Supervisor cancellation")
}

func TestRunStopsConsumptionAndConnectionsBeforeReleasingLock(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	record := func(item string) {
		orderMu.Lock()
		order = append(order, item)
		orderMu.Unlock()
	}
	im := newFakeWeCom()
	im.onStop = func() { record("wecom-closed") }
	supervisor := newFakeRunner()
	supervisor.onStop = func() { record("herdr-closed") }
	handler := &fakeHandler{handled: make(chan wecom.IncomingText, 2)}
	lock := &fakeLock{onRelease: func() { record("lock-released") }}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: handler}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "WeCom Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	cancel()
	im.events <- incoming("late-message", "must-not-run")
	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case message := <-handler.handled:
		t.Fatalf("handled message after cancellation: %#v", message)
	default:
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 || order[2] != "lock-released" {
		t.Fatalf("shutdown order = %#v, want both connections before lock", order)
	}
}

func TestRunBoundsGracefulShutdown(t *testing.T) {
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	releaseRunners := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(releaseRunners)
	stuck := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	supervisor := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			wecom:      &blockingWeCom{blockingRunner: stuck, events: make(chan wecom.IncomingText)},
			supervisor: supervisor,
			handler:    &fakeHandler{},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, stuck.started, "stuck WeCom Run")
	waitClosed(t, supervisor.started, "stuck Supervisor Run")
	cancel()

	err := waitResult(t, result)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 {
		t.Fatalf("lock releases = %d, want 0 while timed-out components may still run", got)
	}
	if !retainsFakeLock(lock) {
		t.Fatal("timed-out lock is not strongly retained")
	}
	releaseRunners()
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunLogsOnlySafeIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	unsafeError := errors.New("secret-sensitive complete-prompt complete-terminal")
	options := testOptions(t)
	options.Stderr = &logs
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		im := newFakeWeCom()
		supervisor := newFakeRunner()
		supervisor.result = unsafeError
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	if err := Run(context.Background(), options); !errors.Is(err, unsafeError) {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	for _, sensitive := range []string{"secret-sensitive", "bot-sensitive", "user-sensitive", "complete-prompt", "complete-terminal"} {
		if strings.Contains(output, sensitive) {
			t.Fatalf("log contains sensitive value %q: %s", sensitive, output)
		}
	}
	if !strings.Contains(output, "bot_hash=") || !strings.Contains(output, "user_hash=") {
		t.Fatalf("safe identifier hashes missing from log: %s", output)
	}
}

func TestSafeErrorTypeUsesFixedSemanticCategories(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "退出超时", err: errors.Join(errors.New("secret"), ErrShutdownTimeout), want: "shutdown_timeout"},
		{name: "循环停止", err: fmt.Errorf("wrapped: %w", ErrLoopStopped), want: "loop_stopped"},
		{name: "上下文", err: context.Canceled, want: "context"},
		{name: "通知队列", err: bridge.ErrNotificationQueueFull, want: "notification_queue_full"},
		{name: "Herdr 协议", err: herdr.ErrProtocolMismatch, want: "herdr_protocol"},
		{name: "Herdr 不可用", err: herdr.ErrUnavailable, want: "herdr_unavailable"},
		{name: "企业微信协议", err: wecom.ErrProtocol, want: "wecom_protocol"},
		{name: "企业微信不可用", err: wecom.ErrUnavailable, want: "wecom_unavailable"},
		{name: "其它错误", err: errors.New("secret"), want: "runtime_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeErrorType(test.err); got != test.want {
				t.Fatalf("safeErrorType() = %q, want %q", got, test.want)
			}
		})
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	dependencies := defaultAppDependencies()
	dependencies.loadConfig = func(string, func(string) string) (config.Config, error) { return testConfig(), nil }
	dependencies.userCacheDir = func() (string, error) { return "/cache", nil }
	dependencies.mkdirAll = func(string, os.FileMode) error { return nil }
	dependencies.acquireLock = func(string) (processLock, error) { return &fakeLock{}, nil }
	dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return "/tmp/test.sock", nil
	}
	dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) { return canceledRuntime(), nil }
	return Options{
		ConfigPath:   "/tmp/config.json",
		Getenv:       func(string) string { return "secret-sensitive" },
		Runner:       fakeCommandRunner{},
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		dependencies: dependencies,
	}
}

func testConfig() config.Config {
	return config.Config{
		WeCom: config.WeComConfig{BotID: "bot-sensitive", AllowedUserID: "user-sensitive", Secret: "secret-sensitive"},
		Herdr: config.HerdrConfig{Session: "named-session"},
		Log:   config.LogConfig{Level: "info"},
	}
}

func canceledRuntime() *applicationRuntime {
	return &applicationRuntime{wecom: newFakeWeCom(), supervisor: newFakeRunner(), handler: &fakeHandler{}}
}

func incoming(messageID, content string) wecom.IncomingText {
	return wecom.IncomingText{
		RequestID: "request-" + messageID, MessageID: messageID, BotID: "bot-sensitive",
		UserID: "user-sensitive", ChatType: "single", Content: content,
	}
}

type fakeCommandRunner struct{}

func (fakeCommandRunner) Output(context.Context, string, ...string) ([]byte, error) { return nil, nil }

type fakeLock struct {
	onRelease func()
	releases  atomic.Int32
}

func (l *fakeLock) Release() error {
	l.releases.Add(1)
	if l.onRelease != nil {
		l.onRelease()
	}
	return nil
}

type fakeRunner struct {
	started chan struct{}
	stopped chan struct{}
	result  error
	onStop  func()
	once    sync.Once
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(chan struct{}), stopped: make(chan struct{})}
}

func (r *fakeRunner) Run(ctx context.Context) error {
	r.once.Do(func() { close(r.started) })
	if r.result != nil {
		return r.result
	}
	<-ctx.Done()
	if r.onStop != nil {
		r.onStop()
	}
	close(r.stopped)
	return ctx.Err()
}

type fakeWeCom struct {
	*fakeRunner
	events chan wecom.IncomingText
	mu     sync.Mutex
	sent   []string
}

func newFakeWeCom() *fakeWeCom {
	return &fakeWeCom{fakeRunner: newFakeRunner(), events: make(chan wecom.IncomingText, 8)}
}

func (w *fakeWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *fakeWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *fakeWeCom) SendMarkdown(_ context.Context, content string) error {
	w.mu.Lock()
	w.sent = append(w.sent, content)
	w.mu.Unlock()
	return nil
}

type fakeHandler struct {
	handled chan wecom.IncomingText
}

func (h *fakeHandler) HandleMessage(_ context.Context, message wecom.IncomingText) {
	if h.handled != nil {
		h.handled <- message
	}
}

type blockingRunner struct {
	started chan struct{}
	unblock <-chan struct{}
	once    sync.Once
}

func (r *blockingRunner) Run(context.Context) error {
	r.once.Do(func() { close(r.started) })
	<-r.unblock
	return nil
}

type blockingWeCom struct {
	*blockingRunner
	events <-chan wecom.IncomingText
}

func (w *blockingWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *blockingWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *blockingWeCom) SendMarkdown(context.Context, string) error { return nil }

type orderedResultWeCom struct {
	events      chan wecom.IncomingText
	fatal       error
	closeEvents bool
	returnAfter <-chan struct{}
}

func newOrderedResultWeCom(fatal error) *orderedResultWeCom {
	return &orderedResultWeCom{events: make(chan wecom.IncomingText), fatal: fatal}
}

func (w *orderedResultWeCom) Run(ctx context.Context) error {
	if w.closeEvents {
		close(w.events)
	}
	if w.returnAfter != nil {
		select {
		case <-w.returnAfter:
		case <-ctx.Done():
			// 结果顺序测试仍需返回预设错误，不能让派生取消覆盖根因。
		}
	}
	if w.fatal != nil {
		return w.fatal
	}
	<-ctx.Done()
	return ctx.Err()
}

func (w *orderedResultWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *orderedResultWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *orderedResultWeCom) SendMarkdown(context.Context, string) error { return nil }

type cancelClosingWeCom struct {
	events  chan wecom.IncomingText
	started chan struct{}
}

func newCancelClosingWeCom() *cancelClosingWeCom {
	return &cancelClosingWeCom{events: make(chan wecom.IncomingText, 1), started: make(chan struct{})}
}

func (w *cancelClosingWeCom) Run(ctx context.Context) error {
	close(w.started)
	<-ctx.Done()
	close(w.events)
	return ctx.Err()
}

func (w *cancelClosingWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *cancelClosingWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *cancelClosingWeCom) SendMarkdown(context.Context, string) error { return nil }

type gatedCancellationRunner struct {
	canceled chan struct{}
	release  <-chan struct{}
}

func newGatedCancellationRunner(release <-chan struct{}) *gatedCancellationRunner {
	return &gatedCancellationRunner{canceled: make(chan struct{}), release: release}
}

func (r *gatedCancellationRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	close(r.canceled)
	<-r.release
	return ctx.Err()
}

type fakeManagedHerdr struct {
	mu       sync.Mutex
	snapshot herdr.Snapshot
	prompts  int
	gets     int
	reads    int
}

func newFakeManagedHerdr() *fakeManagedHerdr {
	agent := "codex"
	title := "Agent"
	return &fakeManagedHerdr{snapshot: herdr.Snapshot{
		Version: "test", Protocol: herdr.RequiredProtocol,
		Panes: []herdr.Pane{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, AgentStatus: herdr.AgentStatusWorking,
		}},
		Agents: []herdr.AgentInfo{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, AgentStatus: herdr.AgentStatusWorking,
		}},
	}}
}

func (f *fakeManagedHerdr) CheckCompatible(context.Context) error { return nil }

func (f *fakeManagedHerdr) Snapshot(context.Context) (herdr.Snapshot, error) { return f.snapshot, nil }

func (f *fakeManagedHerdr) Subscribe(context.Context, []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error) {
	return nil, errors.New("not used")
}

func (f *fakeManagedHerdr) GetAgent(context.Context, string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.snapshot.Agents[0], nil
}

func (f *fakeManagedHerdr) ReadRecent(context.Context, string, int) (herdr.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return herdr.ReadResult{PaneID: "pane-1", Text: "recent terminal"}, nil
}

func (f *fakeManagedHerdr) Prompt(context.Context, string, string) error {
	f.mu.Lock()
	f.prompts++
	f.mu.Unlock()
	return nil
}

func (f *fakeManagedHerdr) SendKey(context.Context, string, string) error { return nil }

func (f *fakeManagedHerdr) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

func (f *fakeManagedHerdr) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func (f *fakeManagedHerdr) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func waitClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Run")
		return nil
	}
}

func waitLockReleasedAndUnretained(t *testing.T, lock *fakeLock) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if lock.releases.Load() == 1 && !retainsFakeLock(lock) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock releases = %d, retained = %v, want released and removed", lock.releases.Load(), retainsFakeLock(lock))
}

func retainsFakeLock(lock *fakeLock) bool {
	retainedLocksMu.Lock()
	defer retainedLocksMu.Unlock()
	for _, retained := range retainedLocks {
		candidate, ok := retained.lock.(*fakeLock)
		if ok && candidate == lock {
			return true
		}
	}
	return false
}
