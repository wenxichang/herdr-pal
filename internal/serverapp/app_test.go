package serverapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminserver"
	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestRunWeComEventLoopDoesNotBlockOtherUsers(t *testing.T) {
	weCom := &eventLoopWeCom{events: make(chan im.IncomingText, 2)}
	approval := newBlockingRegistrationApproval("admin-a")
	gateway := newEventLoopGateway()
	router := newEventLoopRouter(t, gateway, approval)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWeComEventLoop(ctx, weCom, router) }()
	t.Cleanup(func() {
		approval.releaseOnce()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runWeComEventLoop did not stop")
		}
	})

	weCom.events <- eventLoopMessage("request-admin", "message-admin", "admin-a", "/apr 1")
	approval.waitStarted(t)
	weCom.events <- eventLoopMessage("request-user", "message-user", "user-b", "/help")

	if !gateway.waitForRequest("request-user", 300*time.Millisecond) {
		t.Fatal("other user was blocked by slow approval")
	}
}

func TestRunWeComEventLoopKeepsSameUserSerialized(t *testing.T) {
	weCom := &eventLoopWeCom{events: make(chan im.IncomingText, 2)}
	approval := newBlockingRegistrationApproval("admin-a")
	gateway := newEventLoopGateway()
	router := newEventLoopRouter(t, gateway, approval)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWeComEventLoop(ctx, weCom, router) }()
	t.Cleanup(func() {
		approval.releaseOnce()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runWeComEventLoop did not stop")
		}
	})

	weCom.events <- eventLoopMessage("request-first", "message-first", "admin-a", "/apr 1")
	approval.waitStarted(t)
	weCom.events <- eventLoopMessage("request-second", "message-second", "admin-a", "/help")
	if gateway.waitForRequest("request-second", 100*time.Millisecond) {
		t.Fatal("same user message bypassed serialized executor")
	}
	approval.releaseOnce()
	if !gateway.waitForRequest("request-second", time.Second) {
		t.Fatal("same user queued message was not processed after release")
	}
}

func TestRunWeComEventLoopQueueFullDoesNotBlockOtherUsers(t *testing.T) {
	weCom := &eventLoopWeCom{events: make(chan im.IncomingText, 3)}
	approval := newBlockingRegistrationApproval("admin-a")
	gateway := newEventLoopGateway()
	releaseBlockedResponse := gateway.blockResponse("request-overflow")
	router := newEventLoopRouterWithCapacity(t, gateway, approval, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWeComEventLoop(ctx, weCom, router) }()
	t.Cleanup(func() {
		approval.releaseOnce()
		releaseBlockedResponse()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runWeComEventLoop did not stop")
		}
	})

	weCom.events <- eventLoopMessage("request-first", "message-first", "admin-a", "/apr 1")
	approval.waitStarted(t)
	weCom.events <- eventLoopMessage("request-overflow", "message-overflow", "admin-a", "/help")
	weCom.events <- eventLoopMessage("request-user", "message-user", "user-b", "/help")
	if !gateway.waitForRequest("request-user", 300*time.Millisecond) {
		t.Fatal("queue-full handling blocked another user")
	}
}

type eventLoopWeCom struct {
	events chan im.IncomingText
}

func (*eventLoopWeCom) Run(context.Context) error { return nil }

func (runtime *eventLoopWeCom) Events() <-chan im.IncomingText { return runtime.events }

type eventLoopGateway struct {
	mu               sync.Mutex
	requests         map[string]chan struct{}
	blockedRequestID string
	blockedResponse  chan struct{}
	blockedOnce      sync.Once
}

func newEventLoopGateway() *eventLoopGateway {
	return &eventLoopGateway{requests: make(map[string]chan struct{})}
}

func (gateway *eventLoopGateway) RespondMarkdown(_ context.Context, requestID, _ string) error {
	gateway.mu.Lock()
	blocked := requestID == gateway.blockedRequestID
	blockedResponse := gateway.blockedResponse
	gateway.mu.Unlock()
	if blocked && blockedResponse != nil {
		<-blockedResponse
	}
	gateway.mu.Lock()
	channel := gateway.requests[requestID]
	if channel == nil {
		channel = make(chan struct{})
		gateway.requests[requestID] = channel
	}
	select {
	case <-channel:
	default:
		close(channel)
	}
	gateway.mu.Unlock()
	return nil
}

func (gateway *eventLoopGateway) blockResponse(requestID string) func() {
	gateway.mu.Lock()
	gateway.blockedRequestID = requestID
	gateway.blockedResponse = make(chan struct{})
	channel := gateway.blockedResponse
	gateway.mu.Unlock()
	return func() {
		gateway.blockedOnce.Do(func() { close(channel) })
	}
}

func (*eventLoopGateway) SendMarkdownTo(context.Context, string, string) error { return nil }

func (gateway *eventLoopGateway) waitForRequest(requestID string, timeout time.Duration) bool {
	gateway.mu.Lock()
	channel := gateway.requests[requestID]
	if channel == nil {
		channel = make(chan struct{})
		gateway.requests[requestID] = channel
	}
	gateway.mu.Unlock()
	select {
	case <-channel:
		return true
	case <-time.After(timeout):
		return false
	}
}

type blockingRegistrationApproval struct {
	adminID string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingRegistrationApproval(adminID string) *blockingRegistrationApproval {
	return &blockingRegistrationApproval{adminID: adminID, started: make(chan struct{}), release: make(chan struct{})}
}

func (approval *blockingRegistrationApproval) IsAdmin(userID string) bool {
	return userID == approval.adminID
}

func (*blockingRegistrationApproval) NotifyPending(context.Context, machinereg.Request) error {
	return nil
}

func (*blockingRegistrationApproval) PrepareList(string) (server.RegistrationApprovalList, error) {
	return server.RegistrationApprovalList{}, nil
}

func (*blockingRegistrationApproval) CommitList(string, server.RegistrationApprovalList) error {
	return nil
}

func (approval *blockingRegistrationApproval) Approve(context.Context, string, []int) (string, error) {
	select {
	case <-approval.started:
	default:
		close(approval.started)
	}
	<-approval.release
	return "批准完成", nil
}

func (*blockingRegistrationApproval) Reject(context.Context, string, []int) (string, error) {
	return "驳回完成", nil
}

func (*blockingRegistrationApproval) Invalidate(string) error { return nil }

func (approval *blockingRegistrationApproval) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-approval.started:
	case <-time.After(time.Second):
		t.Fatal("approval did not start")
	}
}

func (approval *blockingRegistrationApproval) releaseOnce() {
	approval.once.Do(func() { close(approval.release) })
}

type eventLoopRelay struct{}

func (eventLoopRelay) Select(context.Context, string, hprp.Target) error { return nil }

func (eventLoopRelay) Execute(context.Context, string, hprp.Target, im.IncomingText) (server.RelayExecution, error) {
	return server.RelayExecution{}, nil
}

func (eventLoopRelay) SupportsCapability(string, hprp.Target, string) bool { return false }

func (eventLoopRelay) FetchTerminalSnapshot(context.Context, string, hprp.Target, hprp.OutputMode, int) (hprp.TerminalSnapshotResult, error) {
	return hprp.TerminalSnapshotResult{}, nil
}

func newEventLoopRouter(t *testing.T, gateway server.WeComGateway, approval server.RegistrationApprovalHandler) *server.ConversationRouter {
	return newEventLoopRouterWithCapacity(t, gateway, approval, 64)
}

func newEventLoopRouterWithCapacity(t *testing.T, gateway server.WeComGateway, approval server.RegistrationApprovalHandler, capacity int) *server.ConversationRouter {
	t.Helper()
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	router, err := server.NewConversationRouterWithConfig(
		server.ConversationRouterConfig{RegistrationApproval: approval},
		server.NewSessionCatalog(), server.NewUserExecutor(capacity), gateway, eventLoopRelay{}, deduper,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func eventLoopMessage(requestID, messageID, userID, content string) im.IncomingText {
	return im.IncomingText{RequestID: requestID, MessageID: messageID, UserID: userID, ChatType: "single", Content: content}
}

func TestRedactServerRunErrorPreservesCauseAndRemovesConfiguredSecret(t *testing.T) {
	original := fmt.Errorf("%w: upstream rejected configured-secret", ErrConfig)
	redacted := redactServerRunError(original, audit.NewRedactor([]string{"configured-secret"}))
	if !errors.Is(redacted, ErrConfig) || strings.Contains(redacted.Error(), "configured-secret") || !strings.Contains(redacted.Error(), "[REDACTED]") {
		t.Fatalf("redactServerRunError() = %v", redacted)
	}
}

func TestServerAppRoutesImageGateway(t *testing.T) {
	client, err := wecom.NewClient(wecom.ClientConfig{
		Endpoint: "ws://fake", BotID: "bot-1", Secret: "secret-1",
		Dial: func(context.Context, string) (wecom.Socket, error) { return nil, errors.New("unused") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(client).(server.WeComImageGateway); !ok {
		t.Fatal("wecom.Client does not satisfy server.WeComImageGateway")
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	if _, err := newLogger(&bytes.Buffer{}, "verbose", false); err == nil {
		t.Fatal("newLogger() should reject unknown level")
	}
}

func TestNewLoggerVerboseForcesDebugAndRedactsSecret(t *testing.T) {
	var logs bytes.Buffer
	runtimeLogger, err := newLogger(&logs, "error", true, "secret-sensitive")
	if err != nil {
		t.Fatal(err)
	}
	machineKey := "hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	automationToken := "hpa_0123456789abcdef_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	runtimeLogger.Logger.Debug("服务端详细诊断", "reason", "upstream rejected secret-sensitive", "machine_key", machineKey, "automation_token", automationToken)
	output := logs.String()
	for _, want := range []string{"level=DEBUG", "服务端详细诊断", "reason=\"upstream rejected [REDACTED]\""} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "secret-sensitive") || strings.Contains(output, machineKey) || strings.Contains(output, automationToken) {
		t.Fatalf("logs leaked secret: %q", output)
	}
}

func TestRunServerComponentsStopsHTTPAndWeComOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := make(chan string, 5)
	shutdownCalls := 0
	err := runServerComponents(ctx, nil, waitingServerComponents(stopped), func(context.Context) {
		shutdownCalls++
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runServerComponents() error = %v", err)
	}
	if shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdownCalls)
	}
	for range 5 {
		select {
		case <-stopped:
		default:
			t.Fatal("runServerComponents returned before all components stopped")
		}
	}
}

func TestRunServerComponentsLogsFailedComponentWithReason(t *testing.T) {
	for failingIndex, failingName := range []string{"wecom_connection", "wecom_event_loop", "relay_http", "admin", "web_admin"} {
		t.Run(failingName, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			stopped := make(chan string, 4)
			components := waitingServerComponents(stopped)
			components[failingIndex].run = func(context.Context) error { return errors.New("component exhausted") }
			shutdownCalls := 0

			err := runServerComponents(context.Background(), nil, components, func(context.Context) {
				shutdownCalls++
			}, logger)

			if err == nil || !strings.Contains(err.Error(), "component exhausted") {
				t.Fatalf("runServerComponents() error = %v", err)
			}
			if shutdownCalls != 1 {
				t.Fatalf("shutdown calls = %d, want 1", shutdownCalls)
			}
			for range 4 {
				select {
				case <-stopped:
				default:
					t.Fatal("failed component did not trigger full component cleanup")
				}
			}
			output := logs.String()
			for _, want := range []string{"服务端组件异常退出", "component=" + failingName, "error_type=component_exit", "reason=\"component exhausted\""} {
				if !strings.Contains(output, want) {
					t.Fatalf("logs = %q, want %q", output, want)
				}
			}
		})
	}
}

func TestRunServerComponentsTreatsAdminStopAsNormalShutdown(t *testing.T) {
	stopRequested := make(chan struct{})
	close(stopRequested)
	stopped := make(chan string, 5)
	shutdownCalls := 0
	if err := runServerComponents(context.Background(), stopRequested, waitingServerComponents(stopped), func(context.Context) {
		shutdownCalls++
	}, nil); err != nil {
		t.Fatalf("runServerComponents(admin stop) error = %v", err)
	}
	if shutdownCalls != 1 || len(stopped) != 5 {
		t.Fatalf("admin stop cleanup = calls:%d stopped:%d", shutdownCalls, len(stopped))
	}
}

func TestRunServerFailsWhenAdminSocketCannotStartAndClosesRelay(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "hp-serverapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	adminPath := filepath.Join(stateDir, adminserver.DefaultSocketFileName)
	if err := os.WriteFile(adminPath, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := probe.Addr().String()
	probe.Close()
	configPath := filepath.Join(t.TempDir(), "server.json")
	raw := fmt.Sprintf(`{"wecom":{"bot_id":"bot-test","secret":"wecom-secret"},"server":{"listen":%q,"state_dir":%q},"admin":{"listen":"127.0.0.1:0"},"log":{}}`, listenAddress, stateDir)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Run(ctx, Options{
		ConfigPath: configPath,
		Getenv:     func(string) string { return "wecom-secret" },
		Stderr:     &bytes.Buffer{},
		Stdout:     &bytes.Buffer{},
		AuthFile:   filepath.Join(t.TempDir(), "server-auth.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "HPAP Admin Socket") {
		t.Fatalf("Run() error = %v, want HPAP Admin Socket detail", err)
	}
	listener, listenErr := net.Listen("tcp", listenAddress)
	if listenErr != nil {
		t.Fatalf("Relay listener leaked after Admin startup failure: %v", listenErr)
	}
	listener.Close()
}

func TestRunReportsCorruptAuthFilePathWithoutLeakingContent(t *testing.T) {
	stateDir := secureServerAppTempDir(t)
	authFile := filepath.Join(t.TempDir(), "server-auth.json")
	const sensitiveContent = "sensitive-auth-content"
	if err := os.WriteFile(authFile, []byte(sensitiveContent), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeServerAppConfig(t, stateDir, reserveServerAppAddress(t), "127.0.0.1:0", "bot-auth-corrupt")
	err := Run(context.Background(), Options{
		ConfigPath: configPath, Getenv: func(string) string { return "wecom-secret" },
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, AuthFile: authFile,
	})
	if err == nil || !strings.Contains(err.Error(), authFile) || strings.Contains(err.Error(), sensitiveContent) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunRejectsInvalidRegistrationStoreWithoutLeakingContent(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		mode os.FileMode
	}{
		{name: "corrupt", body: `{"version":1,"registrations":[{"secret":"sensitive-registration-content"}]}`, mode: 0o600},
		{name: "insecure permissions", body: `{"version":1,"registrations":[]}`, mode: 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := secureServerAppTempDir(t)
			registrationPath := filepath.Join(stateDir, "registrations.json")
			if err := os.WriteFile(registrationPath, []byte(test.body), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(registrationPath, test.mode); err != nil {
				t.Fatal(err)
			}
			configPath := writeServerAppConfig(t, stateDir, reserveServerAppAddress(t), "127.0.0.1:0", "bot-registration-invalid")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := Run(ctx, Options{
				ConfigPath: configPath, Getenv: func(string) string { return "" },
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, AuthFile: filepath.Join(t.TempDir(), "server-auth.json"),
			})
			if err == nil || !strings.Contains(err.Error(), "机器注册") || strings.Contains(err.Error(), "sensitive-registration-content") {
				t.Fatalf("Run() error=%v", err)
			}
		})
	}
}

func TestRunPrintsBootstrapOnceAndReleasesRelayAndAdminOnWebListenFailure(t *testing.T) {
	stateDir := secureServerAppTempDir(t)
	relayAddress := reserveServerAppAddress(t)
	occupiedWeb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupiedWeb.Close()
	webAddress := occupiedWeb.Addr().String()
	configPath := writeServerAppConfig(t, stateDir, relayAddress, webAddress, "bot-web-failure")
	authFile := filepath.Join(t.TempDir(), "server-auth.json")

	var firstStdout, firstStderr bytes.Buffer
	err = Run(context.Background(), Options{
		ConfigPath: configPath, Getenv: func(string) string { return "wecom-secret" },
		Stdout: &firstStdout, Stderr: &firstStderr, AuthFile: authFile,
	})
	if err == nil || !strings.Contains(err.Error(), "Web 管理地址") {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Count(firstStdout.String(), "初始密码：") != 1 || strings.Count(firstStdout.String(), "自动化 Token：") != 1 || !strings.Contains(firstStdout.String(), "管理员：admin") {
		t.Fatalf("bootstrap stdout = %q", firstStdout.String())
	}
	for _, secret := range bootstrapOutputSecrets(firstStdout.String()) {
		if secret == "" || strings.Contains(firstStderr.String(), secret) {
			t.Fatalf("bootstrap secret leaked to logs: stdout=%q stderr=%q", firstStdout.String(), firstStderr.String())
		}
	}
	probe, listenErr := net.Listen("tcp", relayAddress)
	if listenErr != nil {
		t.Fatalf("Relay listener leaked after Web failure: %v", listenErr)
	}
	probe.Close()
	adminProbe, listenErr := adminserver.Listen(adminserver.ListenerConfig{StateDir: stateDir})
	if listenErr != nil {
		t.Fatalf("HPAP listener leaked after Web failure: %v", listenErr)
	}
	adminProbe.Close()

	var secondStdout bytes.Buffer
	err = Run(context.Background(), Options{
		ConfigPath: configPath, Getenv: func(string) string { return "wecom-secret" },
		Stdout: &secondStdout, Stderr: &bytes.Buffer{}, AuthFile: authFile,
	})
	if err == nil || secondStdout.Len() != 0 {
		t.Fatalf("second Run() error=%v stdout=%q", err, secondStdout.String())
	}
}

func waitingServerComponents(stopped chan<- string) []serverComponent {
	names := []string{"wecom_connection", "wecom_event_loop", "relay_http", "admin", "web_admin"}
	components := make([]serverComponent, 0, len(names))
	for _, name := range names {
		componentName := name
		components = append(components, serverComponent{name: componentName, run: func(ctx context.Context) error {
			<-ctx.Done()
			stopped <- componentName
			return ctx.Err()
		}})
	}
	return components
}

func secureServerAppTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "hp-serverapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func reserveServerAppAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func writeServerAppConfig(t *testing.T, stateDir, relayAddress, webAddress, botID string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "server.json")
	raw := fmt.Sprintf(`{"wecom":{"bot_id":%q,"secret":"wecom-secret"},"server":{"listen":%q,"state_dir":%q},"admin":{"listen":%q},"log":{}}`, botID, relayAddress, stateDir, webAddress)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func bootstrapOutputSecrets(output string) []string {
	secrets := make([]string, 0, 2)
	for _, line := range strings.Split(output, "\n") {
		for _, prefix := range []string{"初始密码：", "自动化 Token："} {
			if strings.HasPrefix(line, prefix) {
				secrets = append(secrets, strings.TrimPrefix(line, prefix))
			}
		}
	}
	return secrets
}
