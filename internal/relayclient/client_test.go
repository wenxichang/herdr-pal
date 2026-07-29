package relayclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestClientConnectsReportsSnapshotAndExecutesRequests(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	logs := &relayLockedLogBuffer{}
	outbound := &relayOutboundRecorder{pushes: make(chan hprp.CommandOutput, 1)}
	if err := hub.SetOutboundSink(outbound); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{targets: []session.Target{{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
		DisplayAgent: "Codex", Title: "title", Status: herdr.AgentStatusIdle, Workspace: "workspace", Tab: "main",
	}}, pushContent: "later"}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	eventuallyClient(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	if err := hub.Select(context.Background(), "user-a", target); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	result, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "prompt"})
	if err != nil || result.StructuredContent == nil || result.StructuredContent.Text != "handled: prompt" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if selected := executor.Selected(); selected.PaneID != "pane-1" || selected.OccupantHash != "occ-1" {
		t.Fatalf("selected = %#v", selected)
	}
	select {
	case push := <-outbound.pushes:
		if push.Content.Text != "later" || push.Target != target || !push.Final {
			t.Fatalf("push = %#v, want target %#v", push, target)
		}
	case <-time.After(time.Second):
		t.Fatal("missing execute push")
	}
	eventuallyClient(t, func() bool { return strings.Contains(logs.String(), "HPRP 命令已处理") })
	output := logs.String()
	for _, want := range []string{
		"HPRP 命令已处理", "pane_id=pane-1",
		"request_hash=", "message_hash=", "content_length=6", "result=success",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("交互日志缺少 %q：\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"user-a", "prompt", "later"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("交互日志泄露 %q：\n%s", forbidden, output)
		}
	}
}

func TestClientReplaysCompletedCommandWithoutRepeatingLocalSideEffects(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{targets: []session.Target{{
		PaneID: "pane-1", OccupantKey: "occ-1", Agent: "codex", Status: herdr.AgentStatusIdle,
	}}}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool { return hub.Catalog().HasSessions("user-a") })
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	message := im.IncomingText{MessageID: "same-message", UserID: "user-a", Content: "prompt"}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := hub.Execute(context.Background(), "user-a", target, message)
		if err != nil || result.StructuredContent == nil || result.StructuredContent.Text != "handled: prompt" {
			t.Fatalf("Execute(%d) = %#v, %v", attempt, result, err)
		}
	}
	if got := executor.HandleCount(); got != 1 {
		t.Fatalf("local handle count = %d, want 1", got)
	}
}

func TestClientReportsChangedSnapshotWithoutWaitingForCalibration(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{targets: []session.Target{{PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Title: "old", Status: herdr.AgentStatusIdle}}}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Display.Title == "old"
	})
	executor.SetTitle("new")
	eventuallyClient(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Display.Title == "new"
	})
}

func TestClientVerboseLogsConnectionAndSnapshotDetailsWithoutSensitiveContent(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{targets: []session.Target{{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
		Title: "private-panel-title", Status: herdr.AgentStatusIdle,
	}}}
	logs := &relayLockedLogBuffer{}
	client, err := New(Config{
		URL: relayClientURL(relayServer) + "?access_token=private-query", Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test-version", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	executor.SetTitle("private-updated-title")
	eventuallyClient(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Display.Title == "private-updated-title"
	})
	eventuallyClient(t, func() bool { return strings.Contains(logs.String(), "snapshot_sequence=2") })

	output := logs.String()
	for _, want := range []string{
		"HPRP 连接中",
		"endpoint=",
		"machine_id=home-mac",
		"HPRP 握手成功",
		"connection_id=",
		"snapshot_interval=1h0m0s",
		"HPRP 连接成功",
		"session_count=1",
		"HPRP 会话快照已确认",
		"snapshot_sequence=2",
		"previous_session_count=1",
		"changed=true",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	for _, forbidden := range []string{"user-a", "private-query", "private-panel-title", "private-updated-title"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, output)
		}
	}
}

func TestClientSynchronizesServerSelectionAfterExecutorRebind(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	rebound := session.Target{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-2", Agent: "codex",
		DisplayAgent: "Codex", Title: "new session", Status: herdr.AgentStatusWorking, Workspace: "workspace", Tab: "main",
	}
	executor := &fakeExecutor{
		targets: []session.Target{{
			PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
			DisplayAgent: "Codex", Title: "old session", Status: herdr.AgentStatusIdle, Workspace: "workspace", Tab: "main",
		}},
		rebindOnHandle: &rebound,
	}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	oldTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	if err := hub.Catalog().SetSelection("user-a", oldTarget); err != nil {
		t.Fatal(err)
	}

	result, err := hub.Execute(context.Background(), "user-a", oldTarget, im.IncomingText{
		MessageID: "message-rebind", UserID: "user-a", Content: "prompt",
	})
	if err != nil || result.StructuredContent == nil || result.StructuredContent.Text != "handled: prompt" || result.SelectedTarget == nil {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if err := hub.Catalog().RebindSelection(context.Background(), "user-a", oldTarget, *result.SelectedTarget); err != nil {
		t.Fatal(err)
	}
	eventuallyClient(t, func() bool {
		selected, selectedErr := hub.Catalog().Selected("user-a")
		return selectedErr == nil && selected.Ref.SessionID == "occ-2"
	})
}

func TestClientUsesReboundTargetForExecutePush(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	outbound := &resolvingRelayOutboundRecorder{
		catalog: hub.Catalog(),
		results: make(chan resolvedPush, 1),
	}
	if err := hub.SetOutboundSink(outbound); err != nil {
		t.Fatal(err)
	}
	rebound := session.Target{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-2", Agent: "codex",
		DisplayAgent: "Codex", Status: herdr.AgentStatusWorking,
	}
	executor := &fakeExecutor{
		targets: []session.Target{{
			PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
			DisplayAgent: "Codex", Status: herdr.AgentStatusIdle,
		}},
		rebindOnHandle: &rebound,
		pushContent:    "later",
	}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	oldTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	if err := hub.Catalog().SetSelection("user-a", oldTarget); err != nil {
		t.Fatal(err)
	}

	if _, err := hub.Execute(context.Background(), "user-a", oldTarget, im.IncomingText{
		MessageID: "message-push-rebind", UserID: "user-a", Content: "prompt",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-outbound.results:
		if result.err != nil {
			t.Fatalf("push target was not present in catalog: %v", result.err)
		}
		if result.push.Target.SessionID != "occ-2" {
			t.Fatalf("push target = %#v, want rebound occupant", result.push.Target)
		}
	case <-time.After(time.Second):
		t.Fatal("missing execute push")
	}
}

func TestClientStrictTLSDoesNotTrustAutoTestCertificate(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{}
	logs := &relayLockedLogBuffer{}
	client, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: false,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := client.Run(ctx); err == nil {
		t.Fatal("Run() should end with context timeout")
	}
	if hub.Catalog().HasMachine("user-a", "home-mac") {
		t.Fatal("strict TLS client unexpectedly connected")
	}
	output := logs.String()
	for _, want := range []string{"HPRP 连接已断开", "stage=dial", "error_type=tls", "reason=", "retry_delay="} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "user-a") {
		t.Fatalf("logs leaked userid: %s", output)
	}
}

func TestClientLogsServerProtocolRejectionCodeAndMessage(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	firstExecutor := &fakeExecutor{}
	first, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "first", PollInterval: time.Hour, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetExecutor(firstExecutor); err != nil {
		t.Fatal(err)
	}
	firstContext, stopFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(firstContext) }()
	defer func() { stopFirst(); <-firstDone }()
	eventuallyClient(t, func() bool { return hub.Catalog().HasMachine("user-a", "home-mac") })

	logs := &relayLockedLogBuffer{}
	second, err := New(Config{
		URL: relayClientURL(relayServer), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "second", PollInterval: time.Hour, SnapshotInterval: time.Hour,
		BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.SetExecutor(&fakeExecutor{}); err != nil {
		t.Fatal(err)
	}
	secondContext, stopSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stopSecond()
	_ = second.Run(secondContext)

	output := logs.String()
	for _, want := range []string{
		"HPRP 连接已断开",
		"stage=dial",
		"error_type=http_status",
		"status_code=409",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "user-a") {
		t.Fatalf("logs leaked userid: %s", output)
	}
}

func TestRelayErrorTypeHandlesMissingError(t *testing.T) {
	if got := relayErrorType(nil); got != "unknown" {
		t.Fatalf("relayErrorType(nil) = %q, want unknown", got)
	}
}

func TestClientSessionTracksNegotiatedCapabilities(t *testing.T) {
	session := &clientSession{}
	if session.supportsCapability(hprp.CapabilityCommandOutputV1) {
		t.Fatal("command.output.v1 should be disabled before hello.server")
	}
	session.setCapabilities([]string{hprp.CapabilityCommandOutputV1})
	if !session.supportsCapability(hprp.CapabilityCommandOutputV1) {
		t.Fatal("command.output.v1 was not enabled after negotiation")
	}
}

func TestRelayErrorLogArgsRedactsUserIDAndEndpointDetails(t *testing.T) {
	const (
		userID = "user-sensitive"
		rawURL = "wss://account:password@relay.internal:9443/ws?access_token=private-query"
	)
	err := withRelayStage("hello.server", &remoteProtocolError{payload: hprp.ProtocolError{
		Error: hprp.Error{Code: hprp.CodeCommandDenied, Message: "拒绝 Key user-sensitive: " + rawURL}, Close: true,
	}})
	output := slogArgsString(relayErrorLogArgs(err, rawURL, userID))
	for _, sensitive := range []string{userID, "account", "password", "private-query"} {
		if strings.Contains(output, sensitive) {
			t.Fatalf("Relay 错误日志参数泄露 %q：%s", sensitive, output)
		}
	}
	for _, want := range []string{"stage hello.server", "error_code command.denied", "wss://relay.internal:9443"} {
		if !strings.Contains(output, want) {
			t.Fatalf("Relay 错误日志参数缺少 %q：%s", want, output)
		}
	}
}

func slogArgsString(args []any) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, fmt.Sprint(arg))
	}
	return strings.Join(parts, " ")
}

func startRelayClientHub(t *testing.T) (*server.ClientHub, *httptest.Server) {
	t.Helper()
	_, record := relayClientTestCredential(t)
	hub, err := server.NewClientHub(server.NewSessionCatalog(), hprpClientVerifier{record: record}, server.HubConfig{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	t.Cleanup(relayServer.Close)
	return hub, relayServer
}

func relayClientTestKey(t *testing.T) string {
	t.Helper()
	token, _ := relayClientTestCredential(t)
	return token
}

func relayClientTestCredential(t *testing.T) (string, credential.Record) {
	t.Helper()
	token, record, err := credential.Issue(1, "user-a", "home-mac", []credential.SourceRule{"127.0.0.1", "::1"}, nil, time.Unix(1, 0), bytes.NewReader(make([]byte, 48)))
	if err != nil {
		t.Fatal(err)
	}
	return token, record
}

func relayClientURL(relayServer *httptest.Server) string {
	return "wss" + strings.TrimPrefix(relayServer.URL, "https")
}

type fakeExecutor struct {
	mu             sync.Mutex
	targets        []session.Target
	selected       fakeSelection
	sink           im.ReplySink
	pushContent    string
	rebindOnHandle *session.Target
	handleCount    int
}

type fakeSelection struct {
	PaneID       string
	OccupantHash string
}

func (executor *fakeExecutor) CurrentTargets() []session.Target {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]session.Target(nil), executor.targets...)
}

func (executor *fakeExecutor) SelectedTarget() (session.Target, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, target := range executor.targets {
		if target.PaneID == executor.selected.PaneID && target.OccupantKey == executor.selected.OccupantHash {
			return target, nil
		}
	}
	return session.Target{}, session.ErrNoSelection
}

func (executor *fakeExecutor) SelectTarget(paneID, occupantHash string) error {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, target := range executor.targets {
		if target.PaneID == paneID && target.OccupantKey == occupantHash {
			executor.selected = fakeSelection{PaneID: paneID, OccupantHash: occupantHash}
			return nil
		}
	}
	return session.ErrListSnapshotExpired
}

func (executor *fakeExecutor) HandleMessage(ctx context.Context, message im.IncomingText) {
	executor.mu.Lock()
	executor.handleCount++
	if executor.rebindOnHandle != nil {
		rebound := *executor.rebindOnHandle
		executor.targets[0] = rebound
		executor.selected = fakeSelection{PaneID: rebound.PaneID, OccupantHash: rebound.OccupantKey}
	}
	executor.mu.Unlock()
	_ = executor.sink.RespondMarkdown(ctx, message.RequestID, "handled: "+message.Content)
	if executor.pushContent != "" {
		_ = executor.sink.SendMarkdown(ctx, executor.pushContent)
	}
}

func (executor *fakeExecutor) ReadTerminalSnapshot(
	context.Context,
	string,
	string,
	im.OutputMode,
	int,
) (im.TerminalContent, error) {
	return im.TerminalContent{}, errors.New("fake terminal snapshot unavailable")
}

func (executor *fakeExecutor) HandleCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.handleCount
}

func (executor *fakeExecutor) Selected() fakeSelection {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.selected
}

func (executor *fakeExecutor) SetTitle(title string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.targets[0].Title = title
}

type discardWriter struct{}

func (discardWriter) Write(data []byte) (int, error) { return len(data), nil }

type relayLockedLogBuffer struct {
	mu      sync.Mutex
	content strings.Builder
}

func (buffer *relayLockedLogBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.content.Write(data)
}

func (buffer *relayLockedLogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.content.String()
}

type relayOutboundRecorder struct {
	pushes chan hprp.CommandOutput
}

func (recorder *relayOutboundRecorder) SendCommandOutput(_ context.Context, _ string, push hprp.CommandOutput) error {
	recorder.pushes <- push
	return nil
}

func (*relayOutboundRecorder) SendNotification(context.Context, string, string, hprp.NotificationEvent) error {
	return nil
}

type resolvedPush struct {
	push hprp.CommandOutput
	err  error
}

type resolvingRelayOutboundRecorder struct {
	catalog *server.SessionCatalog
	results chan resolvedPush
}

func (recorder *resolvingRelayOutboundRecorder) SendCommandOutput(_ context.Context, userID string, push hprp.CommandOutput) error {
	_, err := recorder.catalog.ResolveTarget(userID, push.Target)
	recorder.results <- resolvedPush{push: push, err: err}
	return err
}

func (*resolvingRelayOutboundRecorder) SendNotification(context.Context, string, string, hprp.NotificationEvent) error {
	return nil
}

func eventuallyClient(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
