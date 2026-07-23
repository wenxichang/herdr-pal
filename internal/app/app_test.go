package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
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

func TestRunCancelsOtherLoopsAfterFatalError(t *testing.T) {
	fatal := errors.New("supervisor fatal")
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	supervisor.result = fatal
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{wecom: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want fatal supervisor error", err)
	}
	waitClosed(t, im.stopped, "WeCom cancellation")
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
	t.Cleanup(func() { close(unblock) })
	stuck := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	options := testOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			wecom:      &blockingWeCom{blockingRunner: stuck, events: make(chan wecom.IncomingText)},
			supervisor: &blockingRunner{started: make(chan struct{}), unblock: unblock},
			handler:    &fakeHandler{},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, options)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}
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
}

func (l *fakeLock) Release() error {
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
