package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/notificationprefs"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/server"
)

type notificationBarrier struct {
	router *server.ConversationRouter
	events chan error
}

func (sink *notificationBarrier) SendCommandOutput(ctx context.Context, user string, output hprp.CommandOutput) error {
	return sink.router.SendCommandOutput(ctx, user, output)
}
func (sink *notificationBarrier) SendNotification(ctx context.Context, user, machine string, event hprp.NotificationEvent) error {
	err := sink.router.SendNotification(ctx, user, machine, event)
	sink.events <- err
	return err
}

type notificationRelayHarness struct {
	router  *server.ConversationRouter
	catalog *server.SessionCatalog
	gateway *relayGateway
	client  *integrationRelayClient
	barrier *notificationBarrier
	store   *notificationprefs.Store
}

func newNotificationRelay(t *testing.T, path string) *notificationRelayHarness {
	t.Helper()
	store, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := server.NewSessionCatalog()
	key, identity := issueIntegrationKey(t, "user-a", "home-mac", 19)
	hub, err := server.NewClientHub(catalog, integrationVerifier{identities: map[string]credential.Identity{key: identity}}, server.HubConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &relayGateway{replies: make(map[string]string), pushes: make(map[string][]string), images: make(map[string][][]byte)}
	deduper, _ := policy.NewDeduper(time.Hour, 100, time.Now)
	router, err := server.NewConversationRouterWithConfig(server.ConversationRouterConfig{NotificationPreferences: store}, catalog, server.NewUserExecutor(64), gateway, hub, deduper, logger)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &notificationBarrier{router: router, events: make(chan error, 16)}
	if err := hub.SetOutboundSink(barrier); err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	t.Cleanup(relayServer.Close)
	client := startIntegrationRelayClient(t, "wss"+strings.TrimPrefix(relayServer.URL, "https"), key, "home-mac", "codex", "集成任务")
	t.Cleanup(func() { client.Stop(t) })
	client.Executor.SetTerminal("notification integration terminal", integrationPNG(t), 1)
	eventuallyRelay(t, func() bool { return catalog.HasSessions("user-a") })
	return &notificationRelayHarness{router: router, catalog: catalog, gateway: gateway, client: client, barrier: barrier, store: store}
}

func (h *notificationRelayHarness) command(id, content string) {
	h.router.Handle(context.Background(), relayIncoming(id, "user-a", content))
}
func (h *notificationRelayHarness) done(t *testing.T) {
	t.Helper()
	target := h.client.Executor.CurrentTargets()[0]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Client.SendNotification(ctx, im.NotificationTarget{PaneID: target.PaneID, OccupantHash: target.OccupantKey}, im.NotificationEvent{Kind: im.NotificationKindAgentStatusChanged, PreviousStatus: "working", Status: "done", OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.barrier.events:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("notification was not processed")
	}
}

func TestNotificationPreferencesEndToEnd(t *testing.T) {
	h := newNotificationRelay(t, filepath.Join(t.TempDir(), "prefs.json"))
	h.command("list", "/ls")
	h.command("off", "/notify off 1,99")
	if reply := h.gateway.Reply("request-off"); !strings.Contains(reply, "已关闭 1 个组合") || !strings.Contains(reply, "跳过 99") {
		t.Fatal(reply)
	}
	pushes := len(h.gateway.Pushes("user-a"))
	h.done(t)
	_, _, count := h.client.Executor.SnapshotStats()
	if count != 0 || len(h.gateway.Pushes("user-a")) != pushes {
		t.Fatal("muted done fetched or delivered output")
	}
	h.command("select", "/1")
	h.command("con", "/con")
	if !strings.Contains(h.gateway.Reply("request-con"), "notification integration terminal") {
		t.Fatal("explicit query suppressed")
	}
	h.command("on", "/notify on all")
	_, _, count = h.client.Executor.SnapshotStats()
	if count != 0 {
		t.Fatal("toggle replayed done")
	}
	h.done(t)
	mode, lines, count := h.client.Executor.SnapshotStats()
	if count != 1 || mode != im.OutputModeText || lines != 100 {
		t.Fatalf("snapshot=%s %d %d", mode, lines, count)
	}
}

func TestNotificationPreferencesSurviveServerReloadAndTargetReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	first := newNotificationRelay(t, path)
	first.command("list", "/ls")
	first.command("off", "/notify off all")
	second := newNotificationRelay(t, path)
	second.client.Executor.mu.Lock()
	second.client.Executor.target.OccupantKey = "replacement-session"
	second.client.Executor.mu.Unlock()
	eventuallyRelay(t, func() bool {
		entries := second.catalog.CatalogEntries("user-a")
		return len(entries) == 1 && entries[0].Ref.SessionID == "replacement-session"
	})
	second.command("list-reload", "/ls")
	if !strings.Contains(second.gateway.Reply("request-list-reload"), "通知：✖️ 关闭") {
		t.Fatal("persisted group did not survive new target")
	}
	second.done(t)
	_, _, count := second.client.Executor.SnapshotStats()
	if count != 0 {
		t.Fatal("replacement did not inherit mute")
	}
	second.client.Executor.mu.Lock()
	second.client.Executor.target.Title = "renamed"
	second.client.Executor.mu.Unlock()
	eventuallyRelay(t, func() bool {
		entries := second.catalog.CatalogEntries("user-a")
		return len(entries) == 1 && entries[0].Session.Display.Title == "renamed"
	})
	second.command("list-renamed", "/ls")
	if !strings.Contains(second.gateway.Reply("request-list-renamed"), "通知：✔️ 开启") {
		t.Fatal("new title not enabled")
	}
	second.done(t)
	_, _, count = second.client.Executor.SnapshotStats()
	if count != 1 {
		t.Fatal("renamed group not notified")
	}
}

func TestNotificationPreferencesProductionServerPersistence(t *testing.T) {
	harness := newHPAPHarness(t)
	key := harness.issueKey(t, "user-a", "home-mac", []string{"127.0.0.1"})
	client := startIntegrationRelayClient(t, harness.relayURL, key.Token, "home-mac", "codex", "persistent task")
	t.Cleanup(func() {
		if client != nil {
			client.Stop(t)
		}
	})
	harness.waitSessions(t, 1)
	harness.wecom.InjectText(t, "notify-list", "user-a", "single", "/ls")
	harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 1)
	harness.wecom.InjectText(t, "notify-off", "user-a", "single", "/notify off 1,99")
	replies := harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 2)
	if !strings.Contains(replies[len(replies)-1].Content, "已关闭") {
		t.Fatal(replies)
	}
	path := filepath.Join(harness.stateDir, "notification_preferences.json")
	store, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	current := client.Executor.CurrentTargets()[0]
	identity, err := notificationprefs.NewIdentity("user-a", "home-mac", hprp.SessionDisplay{Title: current.Title, Workspace: current.Workspace, Tab: current.Tab, Agent: current.Agent})
	if err != nil {
		t.Fatal(err)
	}
	if store.Enabled(identity) {
		data, _ := os.ReadFile(path)
		t.Fatalf("production router not using file: %s", data)
	}
	client.Stop(t)
	client = nil
	harness.stopServerForRestart(t)
	harness.startServer(t)
	harness.waitServerReady(t)
	harness.wecom.WaitSubscribeCount(t, 2)
	client = startIntegrationRelayClient(t, harness.relayURL, key.Token, "home-mac", "codex", "persistent task")
	harness.waitSessions(t, 1)
	harness.wecom.InjectText(t, "notify-list-restart", "user-a", "single", "/ls")
	replies = harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 3)
	if !strings.Contains(replies[len(replies)-1].Content, "通知：✖️ 关闭") {
		t.Fatal("server restart lost preference")
	}
}

func TestNotificationPreferencesListPrunesOnlySyncedOnlineMachines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	h := newNotificationRelay(t, path)
	h.command("list", "/ls")
	h.command("off", "/notify off all")
	oldTarget := h.client.Executor.CurrentTargets()[0]
	oldKey, _ := notificationprefs.NewIdentity("user-a", "home-mac", hprp.SessionDisplay{Title: oldTarget.Title, Workspace: oldTarget.Workspace, Tab: oldTarget.Tab, Agent: oldTarget.Agent})
	offline, _ := notificationprefs.NewIdentity("user-a", "offline-pc", hprp.SessionDisplay{Title: "offline task", Agent: "codex"})
	other := oldKey
	other.PrincipalID = "other-user"
	if err := h.store.SetBatch([]notificationprefs.Identity{offline, other}, false); err != nil {
		t.Fatal(err)
	}
	h.client.Executor.mu.Lock()
	h.client.Executor.target.Title = "new group"
	h.client.Executor.target.OccupantKey = "new occupant"
	h.client.Executor.mu.Unlock()
	eventuallyRelay(t, func() bool {
		entries := h.catalog.CatalogEntries("user-a")
		return len(entries) == 1 && entries[0].Ref.SessionID == "new occupant"
	})
	h.command("reconcile", "/ls")
	reloaded, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reloaded.Lookup(oldKey); exists {
		t.Fatal("expired online group retained")
	}
	for _, key := range []notificationprefs.Identity{offline, other} {
		if enabled, exists := reloaded.Lookup(key); enabled || !exists {
			t.Fatal("unrelated preference lost")
		}
	}
	before := len(h.gateway.Pushes("user-a"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Client.SendNotification(ctx, im.NotificationTarget{PaneID: oldTarget.PaneID, OccupantHash: oldTarget.OccupantKey}, im.NotificationEvent{Kind: im.NotificationKindTargetInvalidated, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.barrier.events:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("late event not processed")
	}
	if len(h.gateway.Pushes("user-a")) != before {
		t.Fatal("late event leaked after cleanup")
	}
}

func TestNotificationPreferencesPreserveMuteAcrossEmptySnapshotAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	h := newNotificationRelay(t, path)
	h.command("list-before-empty", "/ls")
	h.command("mute-before-empty", "/notify off all")
	current := h.client.Executor.CurrentTargets()[0]
	key, err := notificationprefs.NewIdentity("user-a", "home-mac", hprp.SessionDisplay{Title: current.Title, Workspace: current.Workspace, Tab: current.Tab, Agent: current.Agent})
	if err != nil {
		t.Fatal(err)
	}
	h.client.Executor.mu.Lock()
	h.client.Executor.hidden = true
	h.client.Executor.mu.Unlock()
	eventuallyRelay(t, func() bool {
		inventory := h.catalog.NotificationInventory("user-a")
		return len(inventory.MachineIDs) == 1 && len(inventory.Entries) == 0
	})
	h.command("list-during-empty", "/ls")
	reloaded, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if enabled, exists := reloaded.Lookup(key); enabled || !exists {
		t.Fatal("empty snapshot removed persisted mute")
	}
	h.client.Executor.mu.Lock()
	h.client.Executor.hidden = false
	h.client.Executor.mu.Unlock()
	eventuallyRelay(t, func() bool { return len(h.catalog.CatalogEntries("user-a")) == 1 })
	h.command("list-after-empty", "/ls")
	if !strings.Contains(h.gateway.Reply("request-list-after-empty"), "通知：✖️ 关闭") {
		t.Fatal("returning group did not preserve mute")
	}
	before := len(h.gateway.Pushes("user-a"))
	h.done(t)
	_, _, count := h.client.Executor.SnapshotStats()
	if count != 0 || len(h.gateway.Pushes("user-a")) != before {
		t.Fatal("returning muted group fetched or delivered done")
	}
}
