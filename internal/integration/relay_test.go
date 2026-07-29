package integration_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
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
	homeAKey, homeAIdentity := issueIntegrationKey(t, "user-a", "home-mac", 1)
	officeAKey, officeAIdentity := issueIntegrationKey(t, "user-a", "office-pc", 2)
	homeBKey, homeBIdentity := issueIntegrationKey(t, "user-b", "home-mac", 3)
	verifier := integrationVerifier{identities: map[string]credential.Identity{
		homeAKey: homeAIdentity, officeAKey: officeAIdentity, homeBKey: homeBIdentity,
	}}
	hub, err := server.NewClientHub(catalog, verifier, server.HubConfig{}, logger)
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

	homeA := startIntegrationRelayClient(t, endpoint, homeAKey, "home-mac", "codex", "A 的任务")
	officeA := startIntegrationRelayClient(t, endpoint, officeAKey, "office-pc", "codex", "办公室任务")
	homeB := startIntegrationRelayClient(t, endpoint, homeBKey, "home-mac", "codex", "B 的任务")
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
	eventuallyRelayWithDetails(t, func() bool { return homeA.Executor.LastPrompt() == "只发给 A 的 home" }, func() string {
		return "home=" + homeA.Executor.LastPrompt() + ", office=" + officeA.Executor.LastPrompt() + ", user-b=" + homeB.Executor.LastPrompt() +
			", select-reply=" + gateway.Reply("request-select-a") + ", prompt-reply=" + gateway.Reply("request-prompt-a")
	})
	if officeA.Executor.LastPrompt() != "" || homeB.Executor.LastPrompt() != "" {
		t.Fatalf("prompt crossed target: office=%q user-b=%q", officeA.Executor.LastPrompt(), homeB.Executor.LastPrompt())
	}

	router.Handle(context.Background(), relayIncoming("list-b", "user-b", "/ls"))
	router.Handle(context.Background(), relayIncoming("select-b", "user-b", "/1"))
	router.Handle(context.Background(), relayIncoming("prompt-b", "user-b", "只发给 B"))
	eventuallyRelay(t, func() bool { return homeB.Executor.LastPrompt() == "只发给 B" })

	if err := homeA.Client.SendNotification(context.Background(), im.NotificationTarget{
		PaneID: "pane-1", OccupantHash: "occ-home-mac", Agent: "codex", DisplayAgent: "Codex", Title: "A 的任务",
	}, im.NotificationEvent{
		Kind: im.NotificationKindAgentStatusChanged, PreviousStatus: "working", Status: "blocked", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}
	eventuallyRelayWithDetails(t, func() bool {
		messages := strings.Join(gateway.Pushes("user-a"), "\n")
		return strings.Contains(messages, "[home-mac/1] Codex — A 的任务")
	}, func() string {
		return strings.Join(gateway.Pushes("user-a"), "\n")
	})
}

func TestIntegrationImageConCarriesAuditTextAndUploadsPNG(t *testing.T) {
	router, catalog, gateway, client := startSingleIntegrationRelay(t, "opencode")
	client.Executor.SetTerminal("带颜色的审计文本", integrationPNG(t), 3)
	eventuallyRelay(t, func() bool { return catalog.HasSessions("user-a") })
	router.Handle(context.Background(), relayIncoming("image-list", "user-a", "/ls"))
	router.Handle(context.Background(), relayIncoming("image-select", "user-a", "/1"))
	eventuallyRelay(t, func() bool { return len(gateway.Images("user-a")) == 1 })
	if reply := gateway.Reply("request-image-select"); !strings.Contains(reply, "[终端输出#1]") || !strings.Contains(reply, "页码:[3/3]") {
		t.Fatalf("image reply = %q", reply)
	}
	if calls := client.Executor.TerminalCalls(); len(calls) != 1 || calls[0].Mode != im.OutputModeImage || calls[0].Text != "带颜色的审计文本" {
		t.Fatalf("terminal calls = %#v", calls)
	}

	gateway.SetImageError(errors.New("image unavailable"))
	router.Handle(context.Background(), relayIncoming("image-fallback", "user-a", "/con"))
	eventuallyRelay(t, func() bool {
		return strings.Contains(strings.Join(gateway.Pushes("user-a"), "\n"), "带颜色的审计文本")
	})
}

func TestIntegrationPageUpPageDownAcrossModeChanges(t *testing.T) {
	router, catalog, gateway, client := startSingleIntegrationRelay(t, "codex")
	client.Executor.SetTerminal("分页终端", integrationPNG(t), 3)
	eventuallyRelay(t, func() bool { return catalog.HasSessions("user-a") })
	router.Handle(context.Background(), relayIncoming("page-list", "user-a", "/ls"))
	router.Handle(context.Background(), relayIncoming("page-select", "user-a", "/1"))
	router.Handle(context.Background(), relayIncoming("mode-image", "user-a", "/mode img"))
	router.Handle(context.Background(), relayIncoming("page-up", "user-a", "/pageup"))
	router.Handle(context.Background(), relayIncoming("mode-text", "user-a", "/mode txt"))
	router.Handle(context.Background(), relayIncoming("page-down", "user-a", "/pagedn"))

	eventuallyRelay(t, func() bool { return len(client.Executor.TerminalCalls()) == 3 })
	calls := client.Executor.TerminalCalls()
	if calls[0].Mode != im.OutputModeText || calls[0].Page.Current != 3 ||
		calls[1].Mode != im.OutputModeImage || calls[1].Page.Current != 2 ||
		calls[2].Mode != im.OutputModeText || calls[2].Page.Current != 3 {
		t.Fatalf("terminal calls = %#v", calls)
	}
	if reply := gateway.Reply("request-page-down"); !strings.Contains(reply, "分页终端") || !strings.Contains(reply, "页码:[3/3]") {
		t.Fatalf("page down reply = %q", reply)
	}
	if len(gateway.Images("user-a")) != 1 {
		t.Fatalf("images = %d, want 1", len(gateway.Images("user-a")))
	}
}

func TestIntegrationDoneEventLetsServerFetchSnapshot(t *testing.T) {
	router, catalog, gateway, client := startSingleIntegrationRelay(t, "codex")
	client.Executor.SetTerminal("完成后的终端快照", integrationPNG(t), 1)
	eventuallyRelay(t, func() bool { return catalog.HasSessions("user-a") })
	router.Handle(context.Background(), relayIncoming("done-list", "user-a", "/ls"))
	router.Handle(context.Background(), relayIncoming("done-select", "user-a", "/1"))

	if err := client.Client.SendNotification(context.Background(), im.NotificationTarget{
		PaneID: "pane-1", OccupantHash: "occ-home-mac", Agent: "codex", DisplayAgent: "Codex", Title: "集成任务",
	}, im.NotificationEvent{
		Kind: im.NotificationKindAgentStatusChanged, PreviousStatus: "working", Status: "done", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyRelay(t, func() bool {
		return strings.Contains(strings.Join(gateway.Pushes("user-a"), "\n"), "完成后的终端快照")
	})
	mode, maxLines, count := client.Executor.SnapshotStats()
	if count != 1 || mode != im.OutputModeText || maxLines != 100 {
		t.Fatalf("snapshot stats = mode %q max_lines %d count %d", mode, maxLines, count)
	}
}

func startSingleIntegrationRelay(t *testing.T, agent string) (*server.ConversationRouter, *server.SessionCatalog, *relayGateway, *integrationRelayClient) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := server.NewSessionCatalog()
	key, identity := issueIntegrationKey(t, "user-a", "home-mac", 9)
	hub, err := server.NewClientHub(catalog, integrationVerifier{identities: map[string]credential.Identity{key: identity}}, server.HubConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &relayGateway{replies: make(map[string]string), pushes: make(map[string][]string), images: make(map[string][][]byte)}
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
	t.Cleanup(relayServer.Close)
	endpoint := "wss" + strings.TrimPrefix(relayServer.URL, "https")
	client := startIntegrationRelayClient(t, endpoint, key, "home-mac", agent, "集成任务")
	t.Cleanup(func() { client.Stop(t) })
	return router, catalog, gateway, client
}

type integrationRelayClient struct {
	Client   *relayclient.Client
	Executor *integrationRelayExecutor
	cancel   context.CancelFunc
	done     chan error
}

func startIntegrationRelayClient(t *testing.T, endpoint, key, machineID, agent, title string) *integrationRelayClient {
	t.Helper()
	client, err := relayclient.New(relayclient.Config{
		URL: endpoint, Key: key, SkipVerify: true, Version: "integration",
		PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := &integrationRelayExecutor{
		target: session.Target{
			PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-" + machineID,
			Agent: agent, DisplayAgent: integrationDisplayAgent(agent), Title: title, Status: herdr.AgentStatusIdle, Workspace: "workspace", Tab: "main",
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

func integrationDisplayAgent(agent string) string {
	switch agent {
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	default:
		return agent
	}
}

type integrationVerifier struct {
	identities map[string]credential.Identity
}

func (verifier integrationVerifier) VerifyBearer(_ context.Context, token string, _ netip.Addr) (credential.Identity, error) {
	identity, ok := verifier.identities[token]
	if !ok {
		return credential.Identity{}, credential.ErrUnauthenticated
	}
	return identity, nil
}

func issueIntegrationKey(t *testing.T, principalID, machineID string, fill byte) (string, credential.Identity) {
	t.Helper()
	token, record, err := credential.Issue(1, principalID, machineID, []credential.SourceRule{"127.0.0.1", "::1"}, nil, time.Unix(1, 0), bytes.NewReader(bytes.Repeat([]byte{fill}, 48)))
	if err != nil {
		t.Fatal(err)
	}
	return token, credential.Identity{CredentialID: record.CredentialID, PrincipalID: principalID, MachineID: machineID}
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
	mu               sync.Mutex
	target           session.Target
	selected         bool
	prompt           string
	sink             im.ReplySink
	terminal         *im.TerminalContent
	page             int
	terminalCalls    []im.TerminalContent
	snapshotMode     im.OutputMode
	snapshotMaxLines int
	snapshotCount    int
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
	if executor.selected && executor.terminal != nil && isIntegrationTerminalCommand(message.Content) {
		total := executor.terminal.Page.Total
		switch message.Content {
		case "/con":
			executor.page = total
		case "/pageup":
			executor.page = max(1, executor.page-1)
		case "/pagedn":
			executor.page = min(total, executor.page+1)
		}
		content := executor.terminalContentLocked(message.OutputMode, executor.page)
		executor.terminalCalls = append(executor.terminalCalls, content)
		executor.mu.Unlock()
		_ = executor.sink.(im.TerminalReplySink).RespondTerminal(ctx, message.RequestID, content)
		return
	}
	if executor.selected {
		executor.prompt = message.Content
	}
	executor.mu.Unlock()
	_ = executor.sink.RespondMarkdown(ctx, message.RequestID, "已处理")
}

func (executor *integrationRelayExecutor) ReadTerminalSnapshot(
	_ context.Context,
	paneID string,
	occupantHash string,
	mode im.OutputMode,
	maxLines int,
) (im.TerminalContent, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.terminal == nil || executor.target.PaneID != paneID || executor.target.OccupantKey != occupantHash {
		return im.TerminalContent{}, errors.New("integration terminal snapshot unavailable")
	}
	executor.snapshotMode, executor.snapshotMaxLines = mode, maxLines
	executor.snapshotCount++
	return executor.terminalContentLocked(mode, executor.terminal.Page.Total), nil
}

func (executor *integrationRelayExecutor) SetTerminal(text string, pngData []byte, totalPages int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if totalPages < 1 {
		totalPages = 1
	}
	now := time.Now().UTC()
	executor.terminal = &im.TerminalContent{
		Mode: im.OutputModeImage, Text: text, CapturedAt: now,
		Image: &im.TerminalImage{MediaType: "image/png", Data: append([]byte(nil), pngData...), Width: 2, Height: 1, ColorMode: "indexed-256"},
		Page:  &im.TerminalPage{Current: totalPages, Total: totalPages},
	}
	executor.page = totalPages
}

func (executor *integrationRelayExecutor) terminalContentLocked(mode im.OutputMode, page int) im.TerminalContent {
	content := *executor.terminal
	content.Mode = mode
	pageValue := *executor.terminal.Page
	pageValue.Current = page
	content.Page = &pageValue
	if mode == im.OutputModeText {
		content.Image = nil
	} else if executor.terminal.Image != nil {
		imageValue := *executor.terminal.Image
		imageValue.Data = append([]byte(nil), executor.terminal.Image.Data...)
		content.Image = &imageValue
	}
	return content
}

func (executor *integrationRelayExecutor) TerminalCalls() []im.TerminalContent {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]im.TerminalContent(nil), executor.terminalCalls...)
}

func (executor *integrationRelayExecutor) SnapshotStats() (im.OutputMode, int, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.snapshotMode, executor.snapshotMaxLines, executor.snapshotCount
}

func isIntegrationTerminalCommand(content string) bool {
	return content == "/con" || content == "/pageup" || content == "/pagedn"
}

func (executor *integrationRelayExecutor) LastPrompt() string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.prompt
}

type relayGateway struct {
	mu       sync.Mutex
	replies  map[string]string
	pushes   map[string][]string
	images   map[string][][]byte
	imageErr error
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

func (gateway *relayGateway) SendImageTo(_ context.Context, userID string, pngData []byte) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.imageErr != nil {
		return gateway.imageErr
	}
	if gateway.images == nil {
		gateway.images = make(map[string][][]byte)
	}
	gateway.images[userID] = append(gateway.images[userID], append([]byte(nil), pngData...))
	return nil
}

func (gateway *relayGateway) SetImageError(err error) {
	gateway.mu.Lock()
	gateway.imageErr = err
	gateway.mu.Unlock()
}

func (gateway *relayGateway) Images(userID string) [][]byte {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return append([][]byte(nil), gateway.images[userID]...)
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

func integrationPNG(t *testing.T) []byte {
	t.Helper()
	imageData := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
	imageData.SetColorIndex(1, 0, 1)
	var output bytes.Buffer
	if err := png.Encode(&output, imageData); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func eventuallyRelay(t *testing.T, condition func() bool) {
	t.Helper()
	eventuallyRelayWithDetails(t, condition, func() string { return "" })
}

func eventuallyRelayWithDetails(t *testing.T, condition func() bool, details func() string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Relay integration condition was not satisfied: %s", details())
}
