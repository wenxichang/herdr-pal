package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
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
	for _, want := range []string{"1. [home-mac/1] Codex — 实现 Relay", "工作区：herdr-pal / main", "状态：working"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q lacks %q", got, want)
		}
	}
	if relay.CallCount() != 0 {
		t.Fatal("/ls should not reach relay")
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
	if len(calls) != 2 || calls[0].kind != "select" || calls[1].kind != "execute" {
		t.Fatalf("relay calls = %#v", calls)
	}
	for _, call := range calls {
		if call.userID != "user-a" || call.target.MachineID != "home-mac" || call.target.PaneID != "pane-1" || call.target.OccupantHash != "occ-1" {
			t.Fatalf("relay call = %#v", call)
		}
	}
	if calls[1].message.Content != "继续实现" || gateway.LastReply() != "客户端已处理。" {
		t.Fatalf("execute = %#v, reply = %q", calls[1], gateway.LastReply())
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
