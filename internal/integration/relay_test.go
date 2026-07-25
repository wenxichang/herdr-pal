package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestRelayEndToEndRoutesMultipleUsersAndMachines(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := server.NewSessionCatalog()
	hub, err := server.NewClientHub(catalog, server.HubConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &relayGateway{replies: make(map[string]string), pushes: make(map[string][]string)}
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	router, err := server.NewConversationRouter(catalog, server.NewUserExecutor(64), gateway, hub, deduper, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.SetOutboundSink(router); err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	defer relayServer.Close()
	endpoint := "wss" + strings.TrimPrefix(relayServer.URL, "https")

	homeA := startIntegrationRelayClient(t, endpoint, "user-a", "home-mac", "A 的任务")
	officeA := startIntegrationRelayClient(t, endpoint, "user-a", "office-pc", "办公室任务")
	homeB := startIntegrationRelayClient(t, endpoint, "user-b", "home-mac", "B 的任务")
	defer homeA.Stop(t)
	defer officeA.Stop(t)
	defer homeB.Stop(t)

	eventuallyRelay(t, func() bool {
		return len(catalog.CreateNumberedSnapshot("user-a")) == 2 && len(catalog.CreateNumberedSnapshot("user-b")) == 1
	})
	router.Handle(context.Background(), relayIncoming("list-a", "user-a", "/ls"))
	listA := gateway.Reply("request-list-a")
	for _, want := range []string{"[home-mac/1]", "A 的任务", "[office-pc/1]", "办公室任务"} {
		if !strings.Contains(listA, want) {
			t.Fatalf("list %q lacks %q", listA, want)
		}
	}
	router.Handle(context.Background(), relayIncoming("select-a", "user-a", "/1"))
	router.Handle(context.Background(), relayIncoming("prompt-a", "user-a", "只发给 A 的 home"))
	eventuallyRelay(t, func() bool { return homeA.Executor.LastPrompt() == "只发给 A 的 home" })
	if officeA.Executor.LastPrompt() != "" || homeB.Executor.LastPrompt() != "" {
		t.Fatalf("prompt crossed target: office=%q user-b=%q", officeA.Executor.LastPrompt(), homeB.Executor.LastPrompt())
	}

	router.Handle(context.Background(), relayIncoming("list-b", "user-b", "/ls"))
	router.Handle(context.Background(), relayIncoming("select-b", "user-b", "/1"))
	router.Handle(context.Background(), relayIncoming("prompt-b", "user-b", "只发给 B"))
	eventuallyRelay(t, func() bool { return homeB.Executor.LastPrompt() == "只发给 B" })

	if err := homeA.Client.SendNotification(context.Background(), im.NotificationTarget{
		PaneID: "pane-1", OccupantHash: "occ-home-mac", Agent: "codex", DisplayAgent: "Codex", Title: "A 的任务",
	}, "Agent 已阻塞，需要你的处理。"); err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}
	eventuallyRelay(t, func() bool {
		messages := strings.Join(gateway.Pushes("user-a"), "\n")
		return strings.Contains(messages, "[home-mac/1] Codex — A 的任务") && strings.Contains(messages, "Agent 已阻塞")
	})
}

type integrationRelayClient struct {
	Client   *relayclient.Client
	Executor *integrationRelayExecutor
	cancel   context.CancelFunc
	done     chan error
}

func startIntegrationRelayClient(t *testing.T, endpoint, userID, machineID, title string) *integrationRelayClient {
	t.Helper()
	client, err := relayclient.New(relayclient.Config{
		URL: endpoint, UserID: userID, MachineID: machineID, SkipVerify: true, Version: "integration",
		PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := &integrationRelayExecutor{
		target: session.Target{
			PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-" + machineID,
			Agent: "codex", DisplayAgent: "Codex", Title: title, Status: herdr.AgentStatusIdle, Workspace: "workspace", Tab: "main",
		},
		sink: client,
	}
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return &integrationRelayClient{Client: client, Executor: executor, cancel: cancel, done: done}
}

func (client *integrationRelayClient) Stop(t *testing.T) {
	t.Helper()
	client.cancel()
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("Relay client did not stop")
	}
}

type integrationRelayExecutor struct {
	mu       sync.Mutex
	target   session.Target
	selected bool
	prompt   string
	sink     im.ReplySink
}

func (executor *integrationRelayExecutor) CurrentTargets() []session.Target {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return []session.Target{executor.target}
}

func (executor *integrationRelayExecutor) SelectedTarget() (session.Target, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if !executor.selected {
		return session.Target{}, session.ErrNoSelection
	}
	return executor.target, nil
}

func (executor *integrationRelayExecutor) SelectTarget(paneID, occupantHash string) error {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.target.PaneID != paneID || executor.target.OccupantKey != occupantHash {
		return session.ErrListSnapshotExpired
	}
	executor.selected = true
	return nil
}

func (executor *integrationRelayExecutor) HandleMessage(ctx context.Context, message im.IncomingText) {
	executor.mu.Lock()
	if executor.selected {
		executor.prompt = message.Content
	}
	executor.mu.Unlock()
	_ = executor.sink.RespondMarkdown(ctx, message.RequestID, "已处理")
}

func (executor *integrationRelayExecutor) LastPrompt() string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.prompt
}

type relayGateway struct {
	mu      sync.Mutex
	replies map[string]string
	pushes  map[string][]string
}

func (gateway *relayGateway) RespondMarkdown(_ context.Context, requestID, content string) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.replies[requestID] = content
	return nil
}

func (gateway *relayGateway) SendMarkdownTo(_ context.Context, userID, content string) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.pushes[userID] = append(gateway.pushes[userID], content)
	return nil
}

func (gateway *relayGateway) Reply(requestID string) string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.replies[requestID]
}

func (gateway *relayGateway) Pushes(userID string) []string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return append([]string(nil), gateway.pushes[userID]...)
}

func relayIncoming(id, userID, content string) im.IncomingText {
	return im.IncomingText{RequestID: "request-" + id, MessageID: "message-" + id, UserID: userID, ChatType: "single", Content: content}
}

func eventuallyRelay(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Relay integration condition was not satisfied")
}
