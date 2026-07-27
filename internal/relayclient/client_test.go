package relayclient

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestClientConnectsReportsSnapshotAndExecutesRequests(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	outbound := &relayOutboundRecorder{pushes: make(chan relayproto.ExecutePush, 1)}
	if err := hub.SetOutboundSink(outbound); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{targets: []session.Target{{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
		DisplayAgent: "Codex", Title: "title", Status: herdr.AgentStatusIdle, Workspace: "workspace", Tab: "main",
	}}, pushContent: "later"}
	client, err := New(Config{
		URL: relayClientURL(relayServer), UserID: "user-a", MachineID: "home-mac", SkipVerify: true,
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
	defer func() {
		cancel()
		<-done
	}()
	eventuallyClient(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	target := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	if err := hub.Select(context.Background(), "user-a", target); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	result, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "prompt"})
	if err != nil || result.Content != "handled: prompt" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if selected := executor.Selected(); selected.PaneID != "pane-1" || selected.OccupantHash != "occ-1" {
		t.Fatalf("selected = %#v", selected)
	}
	select {
	case push := <-outbound.pushes:
		if push.Content != "later" || push.Target != target {
			t.Fatalf("push = %#v, want target %#v", push, target)
		}
	case <-time.After(time.Second):
		t.Fatal("missing execute push")
	}
}

func TestClientReportsChangedSnapshotWithoutWaitingForCalibration(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{targets: []session.Target{{PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Title: "old", Status: herdr.AgentStatusIdle}}}
	client, err := New(Config{
		URL: relayClientURL(relayServer), UserID: "user-a", MachineID: "home-mac", SkipVerify: true,
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
		return len(entries) == 1 && entries[0].Session.Title == "old"
	})
	executor.SetTitle("new")
	eventuallyClient(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Title == "new"
	})
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
		URL: relayClientURL(relayServer), UserID: "user-a", MachineID: "home-mac", SkipVerify: true,
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
	oldTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	if err := hub.Catalog().SetSelection("user-a", oldTarget); err != nil {
		t.Fatal(err)
	}

	result, err := hub.Execute(context.Background(), "user-a", oldTarget, im.IncomingText{
		MessageID: "message-rebind", UserID: "user-a", Content: "prompt",
	})
	if err != nil || result.Content != "handled: prompt" || result.SelectedTarget == nil {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if err := hub.Catalog().RebindSelection(context.Background(), "user-a", oldTarget, *result.SelectedTarget); err != nil {
		t.Fatal(err)
	}
	eventuallyClient(t, func() bool {
		selected, selectedErr := hub.Catalog().Selected("user-a")
		return selectedErr == nil && selected.Ref.OccupantHash == "occ-2"
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
		URL: relayClientURL(relayServer), UserID: "user-a", MachineID: "home-mac", SkipVerify: true,
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
	oldTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
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
		if result.push.Target.OccupantHash != "occ-2" {
			t.Fatalf("push target = %#v, want rebound occupant", result.push.Target)
		}
	case <-time.After(time.Second):
		t.Fatal("missing execute push")
	}
}

func TestClientStrictTLSDoesNotTrustAutoTestCertificate(t *testing.T) {
	hub, relayServer := startRelayClientHub(t)
	executor := &fakeExecutor{}
	client, err := New(Config{
		URL: relayClientURL(relayServer), UserID: "user-a", MachineID: "home-mac", SkipVerify: false,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
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
}

func startRelayClientHub(t *testing.T) (*server.ClientHub, *httptest.Server) {
	t.Helper()
	hub, err := server.NewClientHub(server.NewSessionCatalog(), server.HubConfig{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	t.Cleanup(relayServer.Close)
	return hub, relayServer
}

func relayClientURL(relayServer *httptest.Server) string {
	return "wss" + strings.TrimPrefix(relayServer.URL, "https")
}

type fakeExecutor struct {
	mu             sync.Mutex
	targets        []session.Target
	selected       relayproto.SessionRef
	sink           im.ReplySink
	pushContent    string
	rebindOnHandle *session.Target
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
	for index, target := range executor.targets {
		if target.PaneID == paneID && target.OccupantKey == occupantHash {
			executor.selected = relayproto.SessionRef{LocalIndex: index + 1, PaneID: paneID, OccupantHash: occupantHash}
			return nil
		}
	}
	return session.ErrListSnapshotExpired
}

func (executor *fakeExecutor) HandleMessage(ctx context.Context, message im.IncomingText) {
	executor.mu.Lock()
	if executor.rebindOnHandle != nil {
		rebound := *executor.rebindOnHandle
		executor.targets[0] = rebound
		executor.selected = relayproto.SessionRef{LocalIndex: 1, PaneID: rebound.PaneID, OccupantHash: rebound.OccupantKey}
	}
	executor.mu.Unlock()
	_ = executor.sink.RespondMarkdown(ctx, message.RequestID, "handled: "+message.Content)
	if executor.pushContent != "" {
		_ = executor.sink.SendMarkdown(ctx, executor.pushContent)
	}
}

func (executor *fakeExecutor) Selected() relayproto.SessionRef {
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

type relayOutboundRecorder struct {
	pushes chan relayproto.ExecutePush
}

func (recorder *relayOutboundRecorder) SendPush(_ context.Context, _ string, push relayproto.ExecutePush) error {
	recorder.pushes <- push
	return nil
}

func (*relayOutboundRecorder) SendNotification(context.Context, string, string, relayproto.Notification) error {
	return nil
}

type resolvedPush struct {
	push relayproto.ExecutePush
	err  error
}

type resolvingRelayOutboundRecorder struct {
	catalog *server.SessionCatalog
	results chan resolvedPush
}

func (recorder *resolvingRelayOutboundRecorder) SendPush(_ context.Context, userID string, push relayproto.ExecutePush) error {
	_, err := recorder.catalog.ResolveTarget(userID, push.Target)
	recorder.results <- resolvedPush{push: push, err: err}
	return err
}

func (*resolvingRelayOutboundRecorder) SendNotification(context.Context, string, string, relayproto.Notification) error {
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
