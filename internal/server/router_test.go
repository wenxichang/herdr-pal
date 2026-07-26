package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestRouterHandlesUserIDWithoutOnlineClient(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	router.Handle(context.Background(), routerMessage("request-1", "message-1", "user-token", "/userid"))
	if got := gateway.LastReply(); got != "user-token" {
		t.Fatalf("reply = %q", got)
	}
	if relay.CallCount() != 0 {
		t.Fatal("/userid should not reach relay")
	}
}

func TestRouterExplainsHowToConnectWhenNoSessions(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-empty", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{Sequence: 1})
	want := "当前没有可用会话，使用/userid 获取用户id，并配置接入herdr-pal，使用/help获取内置命令帮助"
	for index, content := range []string{"/ls", "/1", "/con", "继续处理"} {
		router.Handle(context.Background(), routerMessage(
			"request-empty-"+strconv.Itoa(index), "message-empty-"+strconv.Itoa(index), "user-a", content,
		))
		if got := gateway.LastReply(); got != want {
			t.Fatalf("content %q reply = %q, want %q", content, got, want)
		}
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls = %d, want 0", relay.CallCount())
	}
}

func TestRouterAllowsHelpWhenNoSessions(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	router.Handle(context.Background(), routerMessage("request-help", "message-help", "user-a", "/help"))
	if got := gateway.LastReply(); !strings.Contains(got, "/userid") || !strings.Contains(got, "/help") {
		t.Fatalf("help reply = %q", got)
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls = %d, want 0", relay.CallCount())
	}
}

func TestRouterListsMachinesWithTitleWorkspaceTabAndStatus(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1,
		Sessions: []relayproto.Session{{
			LocalIndex: 1, PaneID: "pane-1", TerminalID: "terminal-1", OccupantHash: "occ-1",
			Agent: "codex", DisplayAgent: "Codex", Title: "实现 Relay", Workspace: "herdr-pal", Tab: "main", Status: "working",
		}},
	})
	router.Handle(context.Background(), routerMessage("request-1", "message-1", "user-a", "/ls"))
	got := gateway.LastReply()
	for _, want := range []string{"1. [home-mac/1] Codex — 实现 Relay", "工作区：herdr-pal/main", "状态：working ⏳"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q lacks %q", got, want)
		}
	}
	if relay.CallCount() != 0 {
		t.Fatal("/ls should not reach relay")
	}
}

func TestRouterListDisplaysEmojiForEveryAgentStatus(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	statuses := []string{"done", "working", "blocked", "idle", "unknown"}
	sessions := make([]relayproto.Session, len(statuses))
	for index, status := range statuses {
		sessions[index] = relaySession(index+1, fmt.Sprintf("pane-%d", index+1), fmt.Sprintf("occ-%d", index+1), status)
		sessions[index].Status = status
	}
	attachSnapshot(t, router.catalog, "conn-status", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: sessions,
	})

	router.Handle(context.Background(), routerMessage("request-status-list", "message-status-list", "user-a", "/ls"))

	reply := gateway.LastReply()
	for _, want := range []string{"状态：done ✅", "状态：working ⏳", "状态：blocked ⁉️", "状态：idle 💤", "状态：unknown ❔"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("status list %q lacks %q", reply, want)
		}
	}
}

func TestRouterSelectsStableTargetBeforeForwardingInput(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	router.Handle(context.Background(), routerMessage("request-ls", "message-ls", "user-a", "/ls"))
	router.Handle(context.Background(), routerMessage("request-select", "message-select", "user-a", "/1"))
	router.Handle(context.Background(), routerMessage("request-prompt", "message-prompt", "user-a", "继续实现"))

	calls := relay.Calls()
	if len(calls) != 3 || calls[0].kind != "select" || calls[1].kind != "execute" || calls[2].kind != "execute" {
		t.Fatalf("relay calls = %#v", calls)
	}
	for _, call := range calls {
		if call.userID != "user-a" || call.target.MachineID != "home-mac" || call.target.PaneID != "pane-1" || call.target.OccupantHash != "occ-1" {
			t.Fatalf("relay call = %#v", call)
		}
	}
	if calls[1].message.Content != "/con" || calls[2].message.Content != "继续实现" || gateway.LastReply() != "客户端已处理。" {
		t.Fatalf("execute = %#v, reply = %q", calls, gateway.LastReply())
	}
}

func TestRouterSelectImmediatelyReturnsDecoratedConsolePage(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "w1:p1", "occ-1", "Panel标题")},
	})
	relay.executeReply = panel.RenderPageWithTotal(session.Target{
		PaneID: "w1:p1", Agent: "codex", DisplayAgent: "Codex", Title: "Panel标题",
		Workspace: "workspace", Tab: "main",
	}, 1, 1, []string{"选择后的终端内容"})

	router.Handle(context.Background(), routerMessage("request-ls", "message-ls", "user-a", "/ls"))
	router.Handle(context.Background(), routerMessage("request-select", "message-select", "user-a", "/1"))

	calls := relay.Calls()
	if len(calls) != 2 || calls[0].kind != "select" || calls[1].kind != "execute" || calls[1].message.Content != "/con" {
		t.Fatalf("relay calls = %#v, want select then /con", calls)
	}
	reply := gateway.LastReply()
	if !strings.HasPrefix(reply, "[终端输出#1]\n```\n选择后的终端内容") || !strings.Contains(reply, "[终端输出#1] [home-mac/1] workspace/main-codex(w1:p1), 页码:[1/1]") {
		t.Fatalf("select reply = %q", reply)
	}
}

func TestRouterTerminalNotificationCreatesNumberedSnapshotWhenListWasNotRequested(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(2, "w1:p1", "occ-1", "后台任务")},
	})
	content := panel.RenderPageWithTotal(session.Target{
		PaneID: "w1:p1", Agent: "codex", Workspace: "workspace", Tab: "main",
	}, 1, 1, []string{"自动编号输出"})

	err := router.SendNotification(context.Background(), "user-a", "home-mac", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 2, PaneID: "w1:p1", OccupantHash: "occ-1"},
		Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply := gateway.LastReply(); !strings.HasPrefix(reply, "[终端输出#1]\n```\n自动编号输出") || !strings.Contains(reply, "[终端输出#1] [home-mac/2]") {
		t.Fatalf("notification reply = %q", reply)
	}

	router.Handle(context.Background(), routerMessage("request-select-auto", "message-select-auto", "user-a", "/1"))
	calls := relay.Calls()
	if len(calls) != 2 || calls[0].kind != "select" || calls[0].target.PaneID != "w1:p1" || calls[1].kind != "execute" {
		t.Fatalf("automatic numbered snapshot calls = %#v", calls)
	}
}

func TestRouterTerminalDecorationRejectsSourceRemovedAfterResolution(t *testing.T) {
	router, _, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	entry, err := router.catalog.ResolveTarget("user-a", relayproto.SessionRef{
		MachineID: "home-mac", LocalIndex: 1, PaneID: "w1:p1", OccupantHash: "occ-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	router.catalog.Detach("conn-1")
	content := panel.RenderPageWithTotal(session.Target{PaneID: "w1:p1", Agent: "codex"}, 1, 1, []string{"旧输出"})

	decorated, err := router.decorateTerminalContent("user-a", entry, content)

	if !errors.Is(err, ErrTargetChanged) || decorated != "" {
		t.Fatalf("decorateTerminalContent() = %q, %v, want ErrTargetChanged", decorated, err)
	}
}

func TestRouterDoesNotStoreSelectionWhenClientRejectsTarget(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	router.Handle(context.Background(), routerMessage("request-ls", "message-ls", "user-a", "/ls"))
	relay.selectErr = ErrTargetChanged
	router.Handle(context.Background(), routerMessage("request-select", "message-select", "user-a", "/sel 1"))
	if _, err := router.catalog.Selected("user-a"); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("Selected() error = %v", err)
	}
	if !strings.Contains(gateway.LastReply(), "目标已变化") {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
}

func TestRouterForwardsClientCommandsUnchanged(t *testing.T) {
	for _, content := range []string{"/con", "/pageup", "/pagedn", "/key down,space", "/enter", "/slash clear"} {
		t.Run(content, func(t *testing.T) {
			router, _, relay := selectedRouterHarness(t)
			router.Handle(context.Background(), routerMessage("request", "message-"+content, "user-a", content))
			calls := relay.Calls()
			if len(calls) != 1 || calls[0].kind != "execute" || calls[0].message.Content != content {
				t.Fatalf("relay calls = %#v", calls)
			}
		})
	}
}

func TestRouterRejectsGroupAndDuplicateMessagesBeforeRelay(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	group := routerMessage("request-group", "message-group", "user-a", "prompt")
	group.ChatType = "group"
	router.Handle(context.Background(), group)
	duplicate := routerMessage("request-1", "message-duplicate", "user-a", "prompt")
	router.Handle(context.Background(), duplicate)
	router.Handle(context.Background(), duplicate)
	if relay.CallCount() != 1 {
		t.Fatalf("relay call count = %d", relay.CallCount())
	}
	if gateway.ReplyCount() != 1 {
		t.Fatalf("reply count = %d", gateway.ReplyCount())
	}
}

func TestRouterExecuteTimeoutDoesNotRetry(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	router.requestTimeout = 10 * time.Millisecond
	relay.execute = func(ctx context.Context, _ string, _ relayproto.SessionRef, _ im.IncomingText) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	router.Handle(context.Background(), routerMessage("request", "message-timeout", "user-a", "slow prompt"))
	if relay.CallCount() != 1 {
		t.Fatalf("relay call count = %d", relay.CallCount())
	}
	if !strings.Contains(gateway.LastReply(), "可能已经提交") {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
}

func TestRouterSendsPushAndStructuredNotificationToOwningUser(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "office-pc"}, relayproto.SessionSnapshot{
		Sequence: 1,
		Sessions: []relayproto.Session{{
			LocalIndex: 2, PaneID: "pane-1", TerminalID: "terminal-1", OccupantHash: "occ-1",
			Agent: "claude", DisplayAgent: "Claude", Title: "修复登录", Workspace: "backend", Tab: "debug", Status: "blocked",
		}},
	})
	if err := router.SendPush(context.Background(), "user-a", relayproto.ExecutePush{
		Target:  relayproto.SessionRef{MachineID: "office-pc", LocalIndex: 2, PaneID: "pane-1", OccupantHash: "occ-1"},
		Content: "后续分段",
	}); err != nil {
		t.Fatalf("SendPush() error = %v", err)
	}
	if err := router.SendNotification(context.Background(), "user-a", "office-pc", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "office-pc", LocalIndex: 2, PaneID: "pane-1", OccupantHash: "occ-1"},
		Content: "Agent 已阻塞，需要你的处理。",
	}); err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}
	replies := gateway.Replies()
	if len(replies) != 2 || replies[0] != "后续分段" {
		t.Fatalf("replies = %#v", replies)
	}
	for _, want := range []string{"[office-pc/2] Claude — 修复登录", "Agent 已阻塞"} {
		if !strings.Contains(replies[1], want) {
			t.Fatalf("notification %q lacks %q", replies[1], want)
		}
	}
}

func TestRouterTerminalPushUsesSourceAndAppendsDifferentCurrentSelection(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-home", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "w1:p1", "occ-home", "后台任务")},
	})
	attachSnapshot(t, router.catalog, "conn-office", ClientKey{UserID: "user-a", MachineID: "office-pc"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(2, "w2:p2", "occ-office", "当前任务")},
	})
	entries := router.catalog.CreateNumberedSnapshot("user-a")
	var selected relayproto.SessionRef
	for _, entry := range entries {
		if entry.Ref.MachineID == "office-pc" {
			selected = entry.Ref
		}
	}
	if err := router.catalog.SetSelection("user-a", selected); err != nil {
		t.Fatal(err)
	}
	content := panel.RenderPageWithTotal(session.Target{
		PaneID: "w1:p1", Agent: "codex", DisplayAgent: "Codex", Title: "后台任务",
		Workspace: "workspace", Tab: "main",
	}, 1, 2, []string{"后续终端分段"})

	err := router.SendPush(context.Background(), "user-a", relayproto.ExecutePush{
		Target: relayproto.SessionRef{
			MachineID: "home-mac", LocalIndex: 1, PaneID: "w1:p1", OccupantHash: "occ-home",
		},
		Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := gateway.LastReply()
	for _, want := range []string{
		"[终端输出#1] [home-mac/1] workspace/main-codex(w1:p1), 页码:[1/2]",
		"⚠️⚠️⚠️[当前会话] [office-pc/2] workspace/main-codex(w2:p2), 你的输入将不会发送给当前输出的会话，使用 /1 切换到当前输出的会话。",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("terminal push %q lacks %q", reply, want)
		}
	}
}

func TestRouterDropsNotificationForChangedOccupant(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "new-occ", "title")},
	})
	err := router.SendNotification(context.Background(), "user-a", "home-mac", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "old-occ"},
		Content: "stale",
	})
	if !errors.Is(err, ErrTargetChanged) || gateway.ReplyCount() != 0 {
		t.Fatalf("SendNotification() = %v, replies %d", err, gateway.ReplyCount())
	}
}

func TestRouterTerminalNotificationAppendsDifferentCurrentSelection(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-home", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "w1:p1", "occ-home", "后台任务")},
	})
	attachSnapshot(t, router.catalog, "conn-office", ClientKey{UserID: "user-a", MachineID: "office-pc"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(2, "w2:p2", "occ-office", "当前任务")},
	})
	entries := router.catalog.CreateNumberedSnapshot("user-a")
	var selected relayproto.SessionRef
	for _, entry := range entries {
		if entry.Ref.MachineID == "office-pc" {
			selected = entry.Ref
		}
	}
	if err := router.catalog.SetSelection("user-a", selected); err != nil {
		t.Fatal(err)
	}
	content := panel.RenderPageWithTotal(session.Target{
		PaneID: "w1:p1", Agent: "codex", DisplayAgent: "Codex", Title: "后台任务",
		Workspace: "workspace", Tab: "main",
	}, 1, 1, []string{"后台输出"})

	err := router.SendNotification(context.Background(), "user-a", "home-mac", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "w1:p1", OccupantHash: "occ-home"},
		Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := gateway.LastReply()
	for _, want := range []string{
		"[终端输出#1] [home-mac/1] workspace/main-codex(w1:p1), 页码:[1/1]",
		"⚠️⚠️⚠️[当前会话] [office-pc/2] workspace/main-codex(w2:p2), 你的输入将不会发送给当前输出的会话，使用 /1 切换到当前输出的会话。",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("terminal notification %q lacks %q", reply, want)
		}
	}
	if !strings.HasPrefix(reply, "[终端输出#1]\n```\n后台输出") {
		t.Fatalf("terminal output should precede context: %q", reply)
	}
}

func newRouterHarness(t *testing.T) (*ConversationRouter, *routerGateway, *routerRelay) {
	t.Helper()
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &routerGateway{}
	relay := &routerRelay{executeReply: "客户端已处理。"}
	router, err := NewConversationRouter(NewSessionCatalog(), NewUserExecutor(64), gateway, relay, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return router, gateway, relay
}

func selectedRouterHarness(t *testing.T) (*ConversationRouter, *routerGateway, *routerRelay) {
	t.Helper()
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	entry := router.catalog.CreateNumberedSnapshot("user-a")[0]
	if err := router.catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	return router, gateway, relay
}

func routerMessage(requestID, messageID, userID, content string) im.IncomingText {
	return im.IncomingText{RequestID: requestID, MessageID: messageID, UserID: userID, ChatType: "single", Content: content}
}

type routerGateway struct {
	mu      sync.Mutex
	replies []string
}

func (gateway *routerGateway) RespondMarkdown(_ context.Context, _ string, content string) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.replies = append(gateway.replies, content)
	return nil
}

func (gateway *routerGateway) SendMarkdownTo(_ context.Context, _ string, content string) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.replies = append(gateway.replies, content)
	return nil
}

func (gateway *routerGateway) LastReply() string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.replies) == 0 {
		return ""
	}
	return gateway.replies[len(gateway.replies)-1]
}

func (gateway *routerGateway) ReplyCount() int {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return len(gateway.replies)
}

func (gateway *routerGateway) Replies() []string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return append([]string(nil), gateway.replies...)
}

type routerRelayCall struct {
	kind    string
	userID  string
	target  relayproto.SessionRef
	message im.IncomingText
}

type routerRelay struct {
	mu           sync.Mutex
	calls        []routerRelayCall
	selectErr    error
	executeReply string
	execute      func(context.Context, string, relayproto.SessionRef, im.IncomingText) (string, error)
}

func (relay *routerRelay) Select(_ context.Context, userID string, target relayproto.SessionRef) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.calls = append(relay.calls, routerRelayCall{kind: "select", userID: userID, target: target})
	return relay.selectErr
}

func (relay *routerRelay) Execute(ctx context.Context, userID string, target relayproto.SessionRef, message im.IncomingText) (string, error) {
	relay.mu.Lock()
	relay.calls = append(relay.calls, routerRelayCall{kind: "execute", userID: userID, target: target, message: message})
	execute := relay.execute
	reply := relay.executeReply
	relay.mu.Unlock()
	if execute != nil {
		return execute(ctx, userID, target, message)
	}
	return reply, nil
}

func (relay *routerRelay) Calls() []routerRelayCall {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return append([]routerRelayCall(nil), relay.calls...)
}

func (relay *routerRelay) CallCount() int { return len(relay.Calls()) }

type testDiscardWriter struct{}

func (testDiscardWriter) Write(data []byte) (int, error) { return len(data), nil }
