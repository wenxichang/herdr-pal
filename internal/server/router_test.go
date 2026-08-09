package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestParseServerActionRegistration(t *testing.T) {
	tests := []struct {
		input   string
		machine string
		sources []string
		wantErr bool
	}{
		{input: "/reg office 192.168.0.1", machine: "office", sources: []string{"192.168.0.1"}},
		{input: "/reg office 192.168.0.1,10.0.0.0/24", machine: "office", sources: []string{"192.168.0.1", "10.0.0.0/24"}},
		{input: "/reg", wantErr: true},
		{input: "/reg office", wantErr: true},
		{input: "/reg office 192.168.0.1 extra", wantErr: true},
		{input: "/reg office 192.168.0.1,", wantErr: true},
	}
	for _, test := range tests {
		action, err := parseServerAction(test.input)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%q accepted", test.input)
			}
			continue
		}
		if err != nil || action.kind != serverActionRegister || action.machineID != test.machine || !reflect.DeepEqual(action.sources, test.sources) {
			t.Fatalf("%q action=%#v err=%v", test.input, action, err)
		}
	}
}

func TestRouterHandlesRegistrationWithoutSessions(t *testing.T) {
	tests := []struct {
		name      string
		result    machinereg.RegisterResult
		err       error
		token     string
		want      string
		forbidden string
	}{
		{
			name: "auto issued", token: "hpk_1_abcdefghijklmnopqrstuvwxyz0123456789ABCDE",
			result: machinereg.RegisterResult{Disposition: machinereg.DispositionAutoIssued, CredentialID: 1},
			want:   "/help", forbidden: "等待管理员审批",
		},
		{
			name:   "pending",
			result: machinereg.RegisterResult{Disposition: machinereg.DispositionPending, Request: &machinereg.Request{RegistrationID: "reg_pending", MachineID: "office"}},
			want:   "等待管理员审批",
		},
		{
			name:   "already pending",
			result: machinereg.RegisterResult{Disposition: machinereg.DispositionAlreadyPending, Request: &machinereg.Request{RegistrationID: "reg_pending", MachineID: "office"}},
			want:   "已有待审批申请",
		},
		{name: "existing machine", err: machinereg.ErrMachineExists, want: "该机器已经注册"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registration := &routerRegistrationRequester{result: test.result, err: test.err, token: test.token}
			router, gateway, relay := newRouterHarnessWithConfig(t, ConversationRouterConfig{RegistrationRequester: registration})
			router.Handle(context.Background(), routerMessage("request-reg-"+strconv.Itoa(index), "message-reg-"+strconv.Itoa(index), "user-a", "/reg office 192.168.0.1"))
			reply := gateway.LastReply()
			if !strings.Contains(reply, test.want) || test.forbidden != "" && strings.Contains(reply, test.forbidden) {
				t.Fatalf("reply=%q", reply)
			}
			if test.token != "" && !strings.Contains(reply, test.token) {
				t.Fatalf("reply does not contain delivered key: %q", reply)
			}
			if registration.calls != 1 || registration.lastInput.PrincipalID != "user-a" || registration.lastInput.MachineID != "office" || !reflect.DeepEqual(registration.lastInput.Sources, []string{"192.168.0.1"}) {
				t.Fatalf("registration=%#v", registration)
			}
			if relay.CallCount() != 0 {
				t.Fatalf("relay calls=%d", relay.CallCount())
			}
		})
	}
}

func TestRouterRegistrationErrorsRemainSpecificWithoutSessions(t *testing.T) {
	router, gateway, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{RegistrationRequester: &routerRegistrationRequester{}})
	router.Handle(context.Background(), routerMessage("request-invalid-reg", "message-invalid-reg", "user-a", "/reg office 192.168.0.1 extra"))
	if reply := gateway.LastReply(); !strings.Contains(reply, "/reg 用法") || reply == noAvailableSessionsMessage {
		t.Fatalf("invalid registration reply=%q", reply)
	}
	router.Handle(context.Background(), routerMessage("request-no-session", "message-no-session", "user-a", "继续"))
	if reply := gateway.LastReply(); reply != noAvailableSessionsMessage {
		t.Fatalf("normal no-session reply=%q", reply)
	}
	for index, content := range []string{"/1 /reg office 127.0.0.1", "#1 /reg office 127.0.0.1"} {
		router.Handle(context.Background(), routerMessage("request-directed-reg-"+strconv.Itoa(index), "message-directed-reg-"+strconv.Itoa(index), "user-a", content))
		if reply := gateway.LastReply(); !strings.Contains(reply, "定向输入不能执行") {
			t.Fatalf("directed registration reply=%q", reply)
		}
	}
}

func TestRouterRegistrationDeliveryFailureDoesNotReplyTwiceOrLogKey(t *testing.T) {
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token := "hpk_9_abcdefghijklmnopqrstuvwxyz0123456789ABCDE"
	registration := &routerRegistrationRequester{
		result: machinereg.RegisterResult{Disposition: machinereg.DispositionAutoIssued, CredentialID: 9},
		token:  token,
	}
	gateway := &registrationFailingGateway{err: errors.New("response unavailable")}
	relay := &routerRelay{capabilities: make(map[string]bool)}
	var logs bytes.Buffer
	router, err := NewConversationRouterWithConfig(
		ConversationRouterConfig{RegistrationRequester: registration},
		NewSessionCatalog(), NewUserExecutor(64), gateway, relay, deduper,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	router.Handle(context.Background(), routerMessage("request-reg-failed", "message-reg-failed", "user-a", "/reg office 192.168.0.1"))
	if gateway.CallCount() != 1 {
		t.Fatalf("response calls=%d", gateway.CallCount())
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls=%d", relay.CallCount())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("logs leaked machine key: %s", logs.String())
	}
}

func TestParseServerActionDoesNotReserveRemovedCommands(t *testing.T) {
	for _, content := range []string{"/userid", "/sel 2"} {
		action, err := parseServerAction(content)
		if err != nil || action.kind != serverActionForward {
			t.Fatalf("parseServerAction(%q) = %#v, %v, want forward", content, action, err)
		}
	}
}

func TestConversationRouterRateLimitsUniqueUserInputBeforeExecution(t *testing.T) {
	now := time.Unix(1000, 0)
	collector := &routerAuditCollector{}
	router, gateway, relay := newRouterHarnessWithConfig(t, ConversationRouterConfig{
		RateLimiter:   NewUserRateLimiter(1, 20, func() time.Time { return now }),
		Auditor:       collector,
		AuditRedactor: audit.NewRedactor([]string{"bot-secret"}),
		BotIDHash:     "bot-hash",
	})
	router.now = func() time.Time { return now }
	router.Handle(context.Background(), routerMessage("request-1", "message-1", "user-a", "/help bot-secret"))
	router.Handle(context.Background(), routerMessage("request-2", "message-2", "user-a", "/help"))
	if reply := gateway.LastReply(); reply != "输入过于频繁，请在 1 秒后重试。" {
		t.Fatalf("reply = %q", reply)
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls = %d", relay.CallCount())
	}
	events := collector.Events()
	if len(events) != 2 || events[0].Outcome != "accepted" || events[1].Outcome != "rate_limited" {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Body != "/help "+audit.RedactedValue || events[0].PrincipalID != "user-a" || events[0].BotIDHash != "bot-hash" {
		t.Fatalf("accepted event = %#v", events[0])
	}
	if events[1].Attributes["limit.window"] != "second" || events[1].Attributes["limit.retry_after_ms"] != "1000" {
		t.Fatalf("limited event = %#v", events[1])
	}
}

func TestConversationRouterDuplicateMessageDoesNotConsumeQuotaOrRepeatAudit(t *testing.T) {
	now := time.Unix(1100, 0)
	collector := &routerAuditCollector{}
	router, gateway, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{
		RateLimiter: NewUserRateLimiter(1, 0, func() time.Time { return now }), Auditor: collector,
	})
	message := routerMessage("request-1", "message-1", "user-a", "/help")
	router.Handle(context.Background(), message)
	router.Handle(context.Background(), message)
	if len(collector.Events()) != 1 || gateway.ReplyCount() != 1 {
		t.Fatalf("events = %#v, replies = %#v", collector.Events(), gateway.Replies())
	}
	now = now.Add(time.Second)
	router.Handle(context.Background(), routerMessage("request-2", "message-2", "user-a", "/help"))
	if len(collector.Events()) != 2 || !strings.Contains(gateway.LastReply(), "Herdr Pal 快速上手") {
		t.Fatalf("events = %#v, reply = %q", collector.Events(), gateway.LastReply())
	}
}

func TestConversationRouterAuditsInvalidAndNoSessionInputs(t *testing.T) {
	collector := &routerAuditCollector{}
	router, _, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{Auditor: collector})
	router.Handle(context.Background(), routerMessage("request-invalid", "message-invalid", "user-a", "/mode png"))
	events := collector.Events()
	if len(events) != 1 || events[0].Action != "invalid" || events[0].Outcome != "accepted" || events[0].Body != "/mode png" {
		t.Fatalf("events = %#v", events)
	}
}

func TestRouterVerboseLogsInteractionWithoutMessageOrUserContent(t *testing.T) {
	router, _, _ := selectedRouterHarness(t)
	logs := &lockedLogBuffer{}
	router.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	router.Handle(context.Background(), routerMessage("private-request-id", "private-message-id", "user-a", "private prompt content"))

	output := logs.String()
	for _, want := range []string{
		"企业微信交互已接收",
		"action=execute",
		"content_bytes=22",
		"企业微信交互路由成功",
		"machine_id=home-mac",
		"pane_id=pane-1",
		"企业微信回复发送成功",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	for _, forbidden := range []string{"private prompt content", "private-request-id", "private-message-id", "user-a"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, output)
		}
	}
}

func TestRouterVerboseLogsExplicitWeComReplyFailure(t *testing.T) {
	router, _, _ := newRouterHarness(t)
	logs := &lockedLogBuffer{}
	router.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router.gateway = failingRouterGateway{err: &wecom.ProtocolError{ErrCode: 93000}}

	router.Handle(context.Background(), routerMessage("request-failure", "message-failure", "user-a", "/help"))

	output := logs.String()
	for _, want := range []string{"企业微信首段回复失败", "error_type=wecom_protocol", "error_code=93000", "reason="} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "user-a") {
		t.Fatalf("logs leaked userid: %s", output)
	}
}

func TestRouterExplainsHowToConnectWhenNoSessions(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-empty", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{Sequence: 1})
	want := noAvailableSessionsMessage
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
	help := gateway.LastReply()
	for _, want := range []string{
		"### Herdr Pal 快速上手",
		"`/N 内容` 在第 N 个会话执行，成功后切换",
		"`#N 内容` 执行但不切换",
		"定向前缀不能用于",
		"`/mode img` 当前会话使用终端图片",
		"`/mode txt` 当前会话使用纯文本",
		"OpenCode 默认使用图片模式",
		"模式只在 Server 本次运行期间保存",
		"/key down,sp,dn,A,7",
		"https://github.com/wenxichang/herdr-pal/releases/latest",
		"herdr-bundle-<版本>-<系统>-<架构>.tar.gz",
		"./install.sh",
		"默认 `~/.local/bin`",
		"机器 Key",
		"Sidecar",
		"live-handoff",
		"~/.config/herdr-pal/config.json",
		"~/.config/herdr/config.toml",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help reply lacks %q:\n%s", want, help)
		}
	}
	for _, forbidden := range []string{"/userid", "/sel", "server-config", "HERDR_PAL_WECOM_SECRET", "herdr-pal-server", "Bot ID"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help reply contains server deployment field %q:\n%s", forbidden, help)
		}
	}
	for _, forbidden := range []string{"herdr-pal-windows-amd64.exe", "创建 config.json", "curl -fsSL https://herdr.dev/install.sh"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help reply retained old installation field %q:\n%s", forbidden, help)
		}
	}
	for _, forbidden := range []string{`"userid":`, `"machine_id":`} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help reply retained self-claimed identity field %q:\n%s", forbidden, help)
		}
	}
	if len(help) > panel.WeComContentLimit {
		t.Fatalf("help reply size = %d, want at most %d bytes", len(help), panel.WeComContentLimit)
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls = %d, want 0", relay.CallCount())
	}
}

func TestRouterHelpReadsProviderForEveryRequest(t *testing.T) {
	provider := &routerHelpProvider{text: "第一版帮助"}
	router, gateway, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{HelpProvider: provider})
	router.Handle(context.Background(), routerMessage("request-help-1", "message-help-1", "user-a", "/help"))
	if got := gateway.LastReply(); got != "第一版帮助" {
		t.Fatalf("first help = %q", got)
	}
	provider.text = "第二版帮助"
	router.Handle(context.Background(), routerMessage("request-help-2", "message-help-2", "user-a", "/help"))
	if got := gateway.LastReply(); got != "第二版帮助" {
		t.Fatalf("second help = %q", got)
	}
}

func TestRouterHelpReportsProviderFailure(t *testing.T) {
	provider := &routerHelpProvider{err: errors.New("permission denied")}
	router, gateway, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{HelpProvider: provider})
	router.Handle(context.Background(), routerMessage("request-help-error", "message-help-error", "user-a", "/help"))
	if got := gateway.LastReply(); !strings.Contains(got, "帮助内容暂时不可用") {
		t.Fatalf("help error reply = %q", got)
	}
}

func TestRouterListsMachinesWithTitleWorkspaceTabAndStatus(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1,
		Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "occ-1", Status: "working",
			Display: hprp.SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex", Title: "实现 Relay", Workspace: "herdr-pal", Tab: "main"},
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
	sessions := make([]hprp.Session, len(statuses))
	for index, status := range statuses {
		sessions[index] = relaySession(index+1, fmt.Sprintf("pane-%d", index+1), fmt.Sprintf("occ-%d", index+1), status)
		sessions[index].Status = status
	}
	attachSnapshot(t, router.catalog, "conn-status", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
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
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	router.Handle(context.Background(), routerMessage("request-ls", "message-ls", "user-a", "/ls"))
	router.Handle(context.Background(), routerMessage("request-select", "message-select", "user-a", "/1"))
	router.Handle(context.Background(), routerMessage("request-prompt", "message-prompt", "user-a", "继续实现"))

	calls := relay.Calls()
	if len(calls) != 3 || calls[0].kind != "select" || calls[1].kind != "execute" || calls[2].kind != "execute" {
		t.Fatalf("relay calls = %#v", calls)
	}
	for _, call := range calls {
		if call.userID != "user-a" || call.target.MachineID != "home-mac" || call.target.SlotID != "pane-1" || call.target.SessionID != "occ-1" {
			t.Fatalf("relay call = %#v", call)
		}
	}
	if calls[1].message.Content != "/con" || calls[2].message.Content != "继续实现" || gateway.LastReply() != "客户端已处理。" {
		t.Fatalf("execute = %#v, reply = %q", calls, gateway.LastReply())
	}
}

func TestRouterSelectImmediatelyReturnsDecoratedConsolePage(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "Panel标题")},
	})
	relay.executeReply.StructuredContent = &hprp.Content{
		Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeText, Text: "选择后的终端内容",
		Page: &hprp.TerminalPage{Current: 1, Total: 1},
	}

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
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(2, "w1:p1", "occ-1", "后台任务")},
	})
	target := hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1"}
	relay.fetchReply = terminalSnapshotResult(target, hprp.OutputModeText, "自动编号输出", 1, 1)
	err := router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(target, "working", "done"))
	if err != nil {
		t.Fatal(err)
	}
	if reply := gateway.LastReply(); !strings.HasPrefix(reply, "[终端输出#1]\n```\n自动编号输出") || !strings.Contains(reply, "[终端输出#1] [home-mac/2]") {
		t.Fatalf("notification reply = %q", reply)
	}

	router.Handle(context.Background(), routerMessage("request-select-auto", "message-select-auto", "user-a", "/1"))
	calls := relay.Calls()
	if len(calls) != 2 || calls[0].kind != "select" || calls[0].target.SlotID != "w1:p1" || calls[1].kind != "execute" {
		t.Fatalf("automatic numbered snapshot calls = %#v", calls)
	}
}

func TestRouterTerminalDecorationRejectsSourceRemovedAfterResolution(t *testing.T) {
	router, _, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	entry, err := router.catalog.ResolveTarget("user-a", hprp.Target{
		MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1",
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
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	router.Handle(context.Background(), routerMessage("request-ls", "message-ls", "user-a", "/ls"))
	relay.selectErr = ErrTargetChanged
	router.Handle(context.Background(), routerMessage("request-select", "message-select", "user-a", "/1"))
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

func TestRouterModeUsesSelectedSessionOnly(t *testing.T) {
	router, gateway, relay, home, office := directedRouterHarness(t)
	relay.capabilities[hprp.CapabilityTerminalImageV1] = true

	router.Handle(context.Background(), routerMessage("request-mode", "message-mode", "user-a", "/mode img"))

	mode, explicit, err := router.catalog.OutputMode("user-a", home.Ref)
	if err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("home mode = %q, %v, %v", mode, explicit, err)
	}
	if _, explicit, err := router.catalog.OutputMode("user-a", office.Ref); err != nil || explicit {
		t.Fatalf("office explicit = %v, error = %v", explicit, err)
	}
	if relay.CallCount() != 0 {
		t.Fatalf("relay calls = %#v, mode should not execute terminal command", relay.Calls())
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "图片模式") || !strings.Contains(reply, "home-mac") {
		t.Fatalf("mode reply = %q", reply)
	}
}

func TestRouterDirectedModeSwitchesOnlyAfterSuccess(t *testing.T) {
	router, gateway, relay, home, office := directedRouterHarness(t)

	router.Handle(context.Background(), routerMessage("request-mode-fail", "message-mode-fail", "user-a", "/2 /mode img"))
	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("selection after failure = %#v, %v", selected, err)
	}
	if !strings.Contains(gateway.LastReply(), "不支持图片模式") {
		t.Fatalf("failure reply = %q", gateway.LastReply())
	}

	relay.capabilities[hprp.CapabilityTerminalImageV1] = true
	router.Handle(context.Background(), routerMessage("request-mode-ok", "message-mode-ok", "user-a", "/2 /mode img"))
	selected, err = router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, office.Ref) {
		t.Fatalf("selection after success = %#v, %v", selected, err)
	}
	mode, explicit, err := router.catalog.OutputMode("user-a", office.Ref)
	if err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("office mode = %q, %v, %v", mode, explicit, err)
	}
}

func TestRouterHashDirectedModeKeepsSelection(t *testing.T) {
	router, _, relay, home, office := directedRouterHarness(t)
	relay.capabilities[hprp.CapabilityTerminalImageV1] = true

	router.Handle(context.Background(), routerMessage("request-mode", "message-mode-hash", "user-a", "#2 /mode img"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("selection = %#v, %v, want home", selected, err)
	}
	mode, explicit, err := router.catalog.OutputMode("user-a", office.Ref)
	if err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("office mode = %q, %v, %v", mode, explicit, err)
	}
}

func TestRouterDefaultsOpenCodeToImageAndOthersToText(t *testing.T) {
	tests := []struct {
		name       string
		agent      string
		display    string
		capability bool
		want       im.OutputMode
	}{
		{name: "opencode", agent: "opencode", capability: true, want: im.OutputModeImage},
		{name: "display opencode", agent: "custom", display: "OpenCode", capability: true, want: im.OutputModeImage},
		{name: "opencode without capability", agent: "opencode", want: im.OutputModeText},
		{name: "codex", agent: "codex", capability: true, want: im.OutputModeText},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router, _, relay := newRouterHarness(t)
			sessionValue := relaySession(1, "pane-1", "occ-1", "title")
			sessionValue.Display.Agent = test.agent
			sessionValue.Display.DisplayAgent = test.display
			attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
				Sequence: 1, Sessions: []hprp.Session{sessionValue},
			})
			entry := router.catalog.CreateNumberedSnapshot("user-a")[0]
			if err := router.catalog.SetSelection("user-a", entry.Ref); err != nil {
				t.Fatal(err)
			}
			relay.capabilities[hprp.CapabilityTerminalImageV1] = test.capability

			router.Handle(context.Background(), routerMessage("request", "message-"+test.name, "user-a", "继续"))

			calls := relay.Calls()
			if len(calls) != 1 || calls[0].message.OutputMode != test.want {
				t.Fatalf("calls = %#v, want mode %q", calls, test.want)
			}
		})
	}
}

func TestParseServerActionSupportsDirectedPrefixes(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		kind        serverActionKind
		index       int
		forward     string
		switchAfter bool
	}{
		{name: "slash prefix", content: "/3 继续处理", kind: serverActionDirected, index: 3, forward: "继续处理", switchAfter: true},
		{name: "hash prefix", content: "#2 /con", kind: serverActionDirected, index: 2, forward: "/con", switchAfter: false},
		{name: "standalone selection", content: "/3", kind: serverActionSelect, index: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, err := parseServerAction(test.content)
			if err != nil {
				t.Fatal(err)
			}
			if action.kind != test.kind || action.index != test.index || action.content != test.forward || action.switchAfter != test.switchAfter {
				t.Fatalf("parseServerAction(%q) = %#v", test.content, action)
			}
		})
	}
}

func TestParseServerActionSupportsModeCommands(t *testing.T) {
	tests := []struct {
		content     string
		kind        serverActionKind
		index       int
		mode        hprp.OutputMode
		switchAfter bool
	}{
		{content: "/mode img", kind: serverActionMode, mode: hprp.OutputModeImage},
		{content: "/mode txt", kind: serverActionMode, mode: hprp.OutputModeText},
		{content: "/2 /mode img", kind: serverActionDirected, index: 2, mode: hprp.OutputModeImage, switchAfter: true},
		{content: "#3 /mode txt", kind: serverActionDirected, index: 3, mode: hprp.OutputModeText},
	}
	for _, test := range tests {
		action, err := parseServerAction(test.content)
		if err != nil {
			t.Fatalf("parseServerAction(%q) error = %v", test.content, err)
		}
		if action.kind != test.kind || action.index != test.index || action.mode != test.mode || action.switchAfter != test.switchAfter {
			t.Fatalf("parseServerAction(%q) = %#v", test.content, action)
		}
	}
	for _, content := range []string{"/mode", "/mode png", "/mode img extra"} {
		if _, err := parseServerAction(content); err == nil {
			t.Fatalf("parseServerAction(%q) should fail", content)
		}
	}
}

func TestParseServerActionRejectsInvalidDirectedPrefixes(t *testing.T) {
	for _, content := range []string{
		"#3",
		"/3 /ls",
		"/3 /help",
		"/3 /2",
		"#3 #2 /con",
	} {
		t.Run(content, func(t *testing.T) {
			if _, err := parseServerAction(content); err == nil {
				t.Fatalf("parseServerAction(%q) should fail", content)
			}
		})
	}
}

func TestRouterDirectedSlashExecutesTargetAndSwitchesAfterSuccess(t *testing.T) {
	router, gateway, relay, home, office := directedRouterHarness(t)

	router.Handle(context.Background(), routerMessage("request-directed", "message-directed", "user-a", "/2 继续处理"))

	calls := relay.Calls()
	if len(calls) != 1 || calls[0].kind != "execute" || !sameSessionRef(calls[0].target, office.Ref) || calls[0].message.Content != "继续处理" {
		t.Fatalf("relay calls = %#v", calls)
	}
	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, office.Ref) {
		t.Fatalf("Selected() = %#v, %v, want office", selected, err)
	}
	if strings.TrimSpace(gateway.LastReply()) != "客户端已处理。" {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
	if sameSessionRef(home.Ref, selected.Ref) {
		t.Fatal("selection did not change")
	}
}

func TestRouterDirectedHashExecutesTargetWithoutSwitching(t *testing.T) {
	router, _, relay, home, office := directedRouterHarness(t)

	router.Handle(context.Background(), routerMessage("request-directed", "message-directed", "user-a", "#2 /con"))

	calls := relay.Calls()
	if len(calls) != 1 || calls[0].kind != "execute" || !sameSessionRef(calls[0].target, office.Ref) || calls[0].message.Content != "/con" {
		t.Fatalf("relay calls = %#v", calls)
	}
	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("Selected() = %#v, %v, want home", selected, err)
	}
}

func TestRouterDirectedSlashKeepsSelectionWhenExecutionFails(t *testing.T) {
	router, gateway, relay, home, _ := directedRouterHarness(t)
	relay.execute = func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error) {
		return RelayExecution{}, ErrTargetChanged
	}

	router.Handle(context.Background(), routerMessage("request-directed", "message-directed", "user-a", "/2 继续处理"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("Selected() = %#v, %v, want home", selected, err)
	}
	if !strings.Contains(gateway.LastReply(), "目标已变化") {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
}

func TestRouterDirectedSlashKeepsSelectionWhenExecutionTimesOut(t *testing.T) {
	router, gateway, relay, home, _ := directedRouterHarness(t)
	router.requestTimeout = 10 * time.Millisecond
	relay.execute = func(ctx context.Context, _ string, _ hprp.Target, _ im.IncomingText) (RelayExecution, error) {
		<-ctx.Done()
		return RelayExecution{}, ctx.Err()
	}

	router.Handle(context.Background(), routerMessage("request-timeout", "message-timeout-directed", "user-a", "/2 继续处理"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("Selected() = %#v, %v, want home", selected, err)
	}
	if !strings.Contains(gateway.LastReply(), "未切换当前会话") {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
}

func TestRouterDirectedSlashSelectsReplacementReturnedByClient(t *testing.T) {
	router, _, relay, _, office := directedRouterHarness(t)
	replacement := office.Ref
	replacement.SessionID = "occ-office-new"
	relay.execute = func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error) {
		if err := router.catalog.ApplySnapshot("conn-office", hprp.SessionSnapshot{
			Sequence: 2, Sessions: []hprp.Session{hprpSession(2, "pane-office", replacement.SessionID, "office-new")},
		}); err != nil {
			t.Fatal(err)
		}
		content := hprp.Content{Type: hprp.ContentTypeText, Text: "会话已切换"}
		return RelayExecution{StructuredContent: &content, SelectedTarget: &replacement}, nil
	}

	router.Handle(context.Background(), routerMessage("request-replacement", "message-replacement", "user-a", "/2 /slash clear"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, replacement) {
		t.Fatalf("Selected() = %#v, %v, want replacement %#v", selected, err, replacement)
	}
}

func TestRouterDirectedHashDoesNotSelectNoncurrentReplacement(t *testing.T) {
	router, _, relay, home, office := directedRouterHarness(t)
	replacement := office.Ref
	replacement.SessionID = "occ-office-new"
	relay.execute = func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error) {
		if err := router.catalog.ApplySnapshot("conn-office", hprp.SessionSnapshot{
			Sequence: 2, Sessions: []hprp.Session{hprpSession(2, "pane-office", replacement.SessionID, "office-new")},
		}); err != nil {
			t.Fatal(err)
		}
		content := hprp.Content{Type: hprp.ContentTypeText, Text: "会话已切换"}
		return RelayExecution{StructuredContent: &content, SelectedTarget: &replacement}, nil
	}

	router.Handle(context.Background(), routerMessage("request-replacement", "message-replacement", "user-a", "#2 /slash clear"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, home.Ref) {
		t.Fatalf("Selected() = %#v, %v, want home", selected, err)
	}
}

func TestRouterDirectedHashKeepsCurrentLogicalSessionAfterReplacement(t *testing.T) {
	router, _, relay := selectedRouterHarness(t)
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-new"}
	relay.execute = func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error) {
		if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
			Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", replacement.SessionID, "new")},
		}); err != nil {
			t.Fatal(err)
		}
		content := hprp.Content{Type: hprp.ContentTypeText, Text: "会话已切换"}
		return RelayExecution{StructuredContent: &content, SelectedTarget: &replacement}, nil
	}

	router.Handle(context.Background(), routerMessage("request-replacement", "message-replacement-hash-current", "user-a", "#1 /slash clear"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, replacement) {
		t.Fatalf("Selected() = %#v, %v, want replacement %#v", selected, err, replacement)
	}
}

func TestRouterCurrentExecutionRebindsReplacementReturnedByClient(t *testing.T) {
	router, _, relay := selectedRouterHarness(t)
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-new"}
	relay.execute = func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error) {
		if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
			Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", replacement.SessionID, "new")},
		}); err != nil {
			t.Fatal(err)
		}
		content := hprp.Content{Type: hprp.ContentTypeText, Text: "会话已切换"}
		return RelayExecution{StructuredContent: &content, SelectedTarget: &replacement}, nil
	}

	router.Handle(context.Background(), routerMessage("request-replacement", "message-replacement", "user-a", "/slash clear"))

	selected, err := router.catalog.Selected("user-a")
	if err != nil || !sameSessionRef(selected.Ref, replacement) {
		t.Fatalf("Selected() = %#v, %v, want replacement %#v", selected, err, replacement)
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
	relay.execute = func(ctx context.Context, _ string, _ hprp.Target, _ im.IncomingText) (RelayExecution, error) {
		<-ctx.Done()
		return RelayExecution{}, ctx.Err()
	}
	router.Handle(context.Background(), routerMessage("request", "message-timeout", "user-a", "slow prompt"))
	if relay.CallCount() != 1 {
		t.Fatalf("relay call count = %d", relay.CallCount())
	}
	if !strings.Contains(gateway.LastReply(), "可能已经提交") {
		t.Fatalf("reply = %q", gateway.LastReply())
	}
}

func TestRouterStatusDoneFetchesSnapshotUsingCurrentMode(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := router.catalog.SetOutputMode("user-a", entry.Ref, hprp.OutputModeText); err != nil {
		t.Fatal(err)
	}
	relay.fetchReply = terminalSnapshotResult(entry.Ref, hprp.OutputModeText, "完成后的终端内容", 1, 3)

	err = router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(
		entry.Ref, "working", "done"))

	if err != nil {
		t.Fatal(err)
	}
	fetches := relay.FetchCalls()
	if len(fetches) != 1 || fetches[0].mode != hprp.OutputModeText || fetches[0].maxLines != 100 || !sameSessionRef(fetches[0].target, entry.Ref) {
		t.Fatalf("fetches = %#v", fetches)
	}
	replies := gateway.Replies()
	if len(replies) != 2 || !strings.Contains(replies[0], "Agent 已完成") || !strings.Contains(replies[1], "完成后的终端内容") || !strings.Contains(replies[1], "页码:[1/3]") {
		t.Fatalf("replies = %#v", replies)
	}
}

func TestRouterRecentBackgroundActivitySendsShortNoticeWithoutFetching(t *testing.T) {
	router, gateway, relay, _, office := directedRouterHarness(t)
	start := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	now := start
	router.now = func() time.Time { return now }
	router.Handle(context.Background(), routerMessage("request-help", "message-help-recent", "user-a", "/help"))
	now = start.Add(time.Minute)

	err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(
		office.Ref, "working", "blocked"))

	if err != nil {
		t.Fatal(err)
	}
	if len(relay.FetchCalls()) != 0 {
		t.Fatalf("fetches = %#v, want none", relay.FetchCalls())
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "有新的输出，等待你的回复") || !strings.Contains(reply, "使用/2切换") {
		t.Fatalf("background notice = %q", reply)
	}
}

func TestRouterSnapshotFailureStillSendsStatusNotification(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	relay.fetchErr = errors.New("snapshot unavailable")

	err = router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(
		entry.Ref, "working", "done"))

	if err != nil {
		t.Fatal(err)
	}
	if replies := gateway.Replies(); len(replies) != 1 || !strings.Contains(replies[0], "Agent 已完成") {
		t.Fatalf("replies = %#v", replies)
	}
}

func TestRouterImageNotificationUsesSameReadTextFallback(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	relay.capabilities[hprp.CapabilityTerminalImageV1] = true
	if err := router.catalog.SetOutputMode("user-a", entry.Ref, hprp.OutputModeImage); err != nil {
		t.Fatal(err)
	}
	relay.fetchReply = hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeFailed,
		Target:  entry.Ref,
		FallbackContent: &hprp.Content{
			Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeText, Text: "图片失败后的同页文本",
			Page: &hprp.TerminalPage{Current: 2, Total: 4},
		},
		Error: &hprp.Error{Code: hprp.CodeTerminalImageFailed},
	}

	err = router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(
		entry.Ref, "working", "blocked"))

	if err != nil {
		t.Fatal(err)
	}
	if len(gateway.Images()) != 0 {
		t.Fatalf("images = %d, want 0", len(gateway.Images()))
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "图片失败后的同页文本") || !strings.Contains(reply, "页码:[2/4]") {
		t.Fatalf("fallback reply = %q", reply)
	}
}

func TestRouterPageUpAndPageDownKeepModeAndPageMetadata(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	relay.capabilities[hprp.CapabilityTerminalImageV1] = true
	if err := router.catalog.SetOutputMode("user-a", entry.Ref, hprp.OutputModeImage); err != nil {
		t.Fatal(err)
	}
	relay.execute = func(_ context.Context, _ string, target hprp.Target, message im.IncomingText) (RelayExecution, error) {
		page := 2
		if message.Content == "/pagedn" {
			page = 3
		}
		content := terminalImageContent("同页审计文本", page, 5)
		return RelayExecution{StructuredContent: &content}, nil
	}

	router.Handle(context.Background(), routerMessage("request-up", "message-up", "user-a", "/pageup"))
	router.Handle(context.Background(), routerMessage("request-down", "message-down", "user-a", "/pagedn"))

	calls := relay.Calls()
	if len(calls) != 2 || calls[0].message.OutputMode != im.OutputModeImage || calls[1].message.OutputMode != im.OutputModeImage {
		t.Fatalf("calls = %#v", calls)
	}
	replies := gateway.Replies()
	if len(replies) != 2 || !strings.Contains(replies[0], "页码:[2/5]") || !strings.Contains(replies[1], "页码:[3/5]") {
		t.Fatalf("replies = %#v", replies)
	}
	if len(gateway.Images()) != 2 {
		t.Fatalf("images = %d, want 2", len(gateway.Images()))
	}
}

func TestRouterTargetInvalidatedDoesNotFetchSnapshot(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2}); err != nil {
		t.Fatal(err)
	}

	err = router.SendNotification(context.Background(), "user-a", "home-mac", invalidatedNotification(entry.Ref))

	if err != nil {
		t.Fatal(err)
	}
	if len(relay.FetchCalls()) != 0 {
		t.Fatalf("fetches = %#v, want none", relay.FetchCalls())
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "目标已失效") || !strings.Contains(reply, "pane-1") {
		t.Fatalf("invalidated reply = %q", reply)
	}
}

func TestRouterSendsPushAndStructuredNotificationToOwningUser(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "office-pc"}, hprp.SessionSnapshot{
		Sequence: 1,
		Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "occ-1", Status: "blocked",
			Display: hprp.SessionDisplay{Index: 2, Agent: "claude", DisplayAgent: "Claude", Title: "修复登录", Workspace: "backend", Tab: "debug"},
		}},
	})
	target := hprp.Target{MachineID: "office-pc", SlotID: "pane-1", SessionID: "occ-1"}
	if err := router.SendCommandOutput(context.Background(), "user-a", routerCommandOutput(target, "后续分段")); err != nil {
		t.Fatalf("SendCommandOutput() error = %v", err)
	}
	if err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(target, "idle", "working")); err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}
	replies := gateway.Replies()
	if len(replies) != 2 || replies[0] != "后续分段" {
		t.Fatalf("replies = %#v", replies)
	}
	for _, want := range []string{"[office-pc/2] Claude — 修复登录", "Agent 开始工作"} {
		if !strings.Contains(replies[1], want) {
			t.Fatalf("notification %q lacks %q", replies[1], want)
		}
	}
}

func TestRouterTerminalPushUsesSourceAndAppendsDifferentCurrentSelection(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-home", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-home", "后台任务")},
	})
	attachSnapshot(t, router.catalog, "conn-office", ClientKey{UserID: "user-a", MachineID: "office-pc"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(2, "w2:p2", "occ-office", "当前任务")},
	})
	entries := router.catalog.CreateNumberedSnapshot("user-a")
	var selected hprp.Target
	for _, entry := range entries {
		if entry.Ref.MachineID == "office-pc" {
			selected = entry.Ref
		}
	}
	if err := router.catalog.SetSelection("user-a", selected); err != nil {
		t.Fatal(err)
	}
	err := router.SendCommandOutput(context.Background(), "user-a", routerTerminalCommandOutput(
		hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-home"}, "后续终端分段", 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	reply := gateway.LastReply()
	for _, want := range []string{
		"[终端输出#1] [home-mac/1] workspace/main-codex(w1:p1), 页码:[1/2]",
		"⚠️⚠️⚠️ 你的输入不会发送到该输出会话，使用 /1 切换到当前输出的会话。",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("terminal push %q lacks %q", reply, want)
		}
	}
}

func TestConversationRouterTerminalAuditRecordsTextReply(t *testing.T) {
	collector := &routerAuditCollector{}
	router, _, relay := newRouterHarnessWithConfig(t, ConversationRouterConfig{Auditor: collector})
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	entry := router.catalog.CreateNumberedSnapshot("user-a")[0]
	if err := router.catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	terminal := hprp.Content{Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeText, Text: "终端原始文本", Page: &hprp.TerminalPage{Current: 1, Total: 1}}
	relay.executeReply = RelayExecution{StructuredContent: &terminal}
	router.Handle(context.Background(), routerMessage("request-1", "message-1", "user-a", "/con"))
	events := terminalAuditEvents(collector.Events())
	if len(events) != 1 {
		t.Fatalf("events = %#v", collector.Events())
	}
	event := events[0]
	if event.Action != "command_result" || event.Outcome != "delivered" || event.Presentation != "txt" || event.Delivery != "reply" || event.Body != "终端原始文本" {
		t.Fatalf("event = %#v", event)
	}
	if event.MachineID != "home-mac" || event.Agent != "codex" || event.PaneID != "w1:p1" || event.SessionIDHash == "" || event.MessageIDHash == "" || event.RequestIDHash == "" {
		t.Fatalf("target event = %#v", event)
	}
}

func TestConversationRouterUserInputAuditRecordsSelectedAgent(t *testing.T) {
	collector := &routerAuditCollector{}
	router, _, _ := newRouterHarnessWithConfig(t, ConversationRouterConfig{Auditor: collector})
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	entry := router.catalog.CreateNumberedSnapshot("user-a")[0]
	if err := router.catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	router.Handle(context.Background(), routerMessage("request-input", "message-input", "user-a", "继续实现"))
	events := collector.Events()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	event := events[0]
	if event.EventName != audit.EventNameUserInput || event.Agent != "codex" || event.MachineID != "home-mac" || event.PaneID != "w1:p1" || event.SessionIDHash == "" {
		t.Fatalf("user input event = %#v", event)
	}
}

func TestConversationRouterTerminalAuditRecordsImagePushAndFallbackText(t *testing.T) {
	tests := []struct {
		name             string
		gateway          WeComGateway
		wantPresentation string
	}{
		{name: "image", gateway: &routerGateway{}, wantPresentation: "img"},
		{name: "fallback", gateway: &imageFallbackGateway{}, wantPresentation: "txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := &routerAuditCollector{}
			deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			router, err := NewConversationRouterWithConfig(ConversationRouterConfig{Auditor: collector}, NewSessionCatalog(), NewUserExecutor(64), test.gateway, &routerRelay{capabilities: make(map[string]bool)}, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
			if err != nil {
				t.Fatal(err)
			}
			attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
				Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
			})
			target := hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1"}
			if err := router.SendCommandOutput(context.Background(), "user-a", hprp.CommandOutput{Target: target, Content: terminalImageContent("图片配套审计文本", 1, 1)}); err != nil {
				t.Fatal(err)
			}
			events := terminalAuditEvents(collector.Events())
			if len(events) != 1 || events[0].Action != "command_output" || events[0].Outcome != "delivered" || events[0].Presentation != test.wantPresentation || events[0].Delivery != "push" || events[0].Body != "图片配套审计文本" {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestConversationRouterTerminalAuditRecordsFinalDeliveryFailure(t *testing.T) {
	collector := &routerAuditCollector{}
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway := failingRouterGateway{err: errors.New("delivery failed")}
	router, err := NewConversationRouterWithConfig(ConversationRouterConfig{Auditor: collector}, NewSessionCatalog(), NewUserExecutor(64), gateway, &routerRelay{capabilities: make(map[string]bool)}, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	target := hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1"}
	err = router.SendCommandOutput(context.Background(), "user-a", routerTerminalCommandOutput(target, "失败前的终端文本", 1, 1))
	if err == nil {
		t.Fatal("SendCommandOutput() error = nil")
	}
	events := terminalAuditEvents(collector.Events())
	if len(events) != 1 || events[0].Outcome != "delivery_failed" || events[0].Presentation != "txt" || events[0].Body != "失败前的终端文本" {
		t.Fatalf("events = %#v", events)
	}
}

func terminalAuditEvents(events []audit.Event) []audit.Event {
	filtered := make([]audit.Event, 0, len(events))
	for _, event := range events {
		if event.EventName == audit.EventNameTerminalOutput {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func TestRouterSerializesOutboundImageAndTextForSameUser(t *testing.T) {
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway := newBlockingImageGateway()
	relay := &routerRelay{capabilities: make(map[string]bool)}
	router, err := NewConversationRouter(NewSessionCatalog(), NewUserExecutor(64), gateway, relay, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	target := hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1"}
	imageDone := make(chan error, 1)
	go func() {
		imageDone <- router.SendCommandOutput(context.Background(), "user-a", hprp.CommandOutput{
			Target: target, Sequence: 1, Final: true, Content: terminalImageContent("图片", 1, 1),
		})
	}()
	<-gateway.imageStarted

	textDone := make(chan error, 1)
	go func() {
		textDone <- router.SendCommandOutput(context.Background(), "user-a", routerCommandOutput(target, "后续文本"))
	}()
	select {
	case err := <-textDone:
		t.Fatalf("text output completed before image pair: %v, events=%#v", err, gateway.Events())
	case <-time.After(100 * time.Millisecond):
	}
	close(gateway.releaseImage)
	if err := <-imageDone; err != nil {
		t.Fatal(err)
	}
	if err := <-textDone; err != nil {
		t.Fatal(err)
	}
	if got := gateway.Events(); !reflect.DeepEqual(got, []string{"header", "image-start", "image-end", "text"}) {
		t.Fatalf("events = %#v", got)
	}
}

func TestRouterDoesNotSendImageWhenReplyHeaderFails(t *testing.T) {
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &failingImageHeaderGateway{err: errors.New("header failed")}
	router, err := NewConversationRouter(NewSessionCatalog(), NewUserExecutor(64), gateway, &routerRelay{capabilities: make(map[string]bool)}, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-1", "任务")},
	})
	entry, err := router.catalog.ResolveTarget("user-a", hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-1"})
	if err != nil {
		t.Fatal(err)
	}
	message := routerMessage("request-1", "message-1", "user-a", "/con")
	if err := router.sendTerminalImageReply(context.Background(), message, entry, terminalImageContent("图片", 1, 1)); !errors.Is(err, gateway.err) {
		t.Fatalf("sendTerminalImageReply() error = %v", err)
	}
	if gateway.imageCalls != 0 {
		t.Fatalf("image calls = %d", gateway.imageCalls)
	}
}

func TestRouterDropsNotificationForChangedOccupant(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "pane-1", "new-occ", "title")},
	})
	err := router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(
		hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "old-occ"}, "working", "done"))
	if !errors.Is(err, ErrTargetChanged) || gateway.ReplyCount() != 0 {
		t.Fatalf("SendNotification() = %v, replies %d", err, gateway.ReplyCount())
	}
}

func TestRouterTerminalNotificationAppendsDifferentCurrentSelection(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-home", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "w1:p1", "occ-home", "后台任务")},
	})
	attachSnapshot(t, router.catalog, "conn-office", ClientKey{UserID: "user-a", MachineID: "office-pc"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(2, "w2:p2", "occ-office", "当前任务")},
	})
	entries := router.catalog.CreateNumberedSnapshot("user-a")
	var selected hprp.Target
	for _, entry := range entries {
		if entry.Ref.MachineID == "office-pc" {
			selected = entry.Ref
		}
	}
	if err := router.catalog.SetSelection("user-a", selected); err != nil {
		t.Fatal(err)
	}
	target := hprp.Target{MachineID: "home-mac", SlotID: "w1:p1", SessionID: "occ-home"}
	relay.fetchReply = terminalSnapshotResult(target, hprp.OutputModeText, "后台输出", 1, 1)
	err := router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(target, "working", "done"))
	if err != nil {
		t.Fatal(err)
	}
	reply := gateway.LastReply()
	for _, want := range []string{
		"[终端输出#1] [home-mac/1] workspace/main-codex(w1:p1), 页码:[1/1]",
		"⚠️⚠️⚠️ 你的输入不会发送到该输出会话，使用 /1 切换到当前输出的会话。",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("terminal notification %q lacks %q", reply, want)
		}
	}
	if !strings.HasPrefix(reply, "[终端输出#1]\n```\n后台输出") {
		t.Fatalf("terminal output should precede context: %q", reply)
	}
}

func TestRouterUsesBriefBackgroundNoticeWhileUserIsActive(t *testing.T) {
	router, gateway, _, _, office := directedRouterHarness(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	router.now = func() time.Time { return now }
	router.Handle(context.Background(), routerMessage("request-help", "message-help-active", "user-a", "/help"))
	now = now.Add(time.Minute)

	err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(office.Ref, "working", "blocked"))
	if err != nil {
		t.Fatal(err)
	}
	reply := gateway.LastReply()
	want := "⚠️ [office-pc/2] workspace/main-codex(pane-office) 有新的输出，等待你的回复，使用/2切换"
	if reply != want {
		t.Fatalf("background notice = %q, want %q", reply, want)
	}
}

func TestRouterBackgroundNoticeRefreshesActivityWindow(t *testing.T) {
	router, gateway, relay, _, office := directedRouterHarness(t)
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	now := start
	router.now = func() time.Time { return now }
	router.Handle(context.Background(), routerMessage("request-help", "message-help-window", "user-a", "/help"))
	relay.fetchReply = terminalSnapshotResult(office.Ref, hprp.OutputModeText, "后台输出", 1, 1)
	notify := func() {
		t.Helper()
		if err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(office.Ref, "working", "blocked")); err != nil {
			t.Fatal(err)
		}
	}

	now = start.Add(2 * time.Minute)
	notify()
	if !strings.Contains(gateway.LastReply(), "有新的输出") {
		t.Fatalf("notice at boundary = %q", gateway.LastReply())
	}
	now = start.Add(3*time.Minute + 30*time.Second)
	notify()
	if !strings.Contains(gateway.LastReply(), "有新的输出") {
		t.Fatalf("sliding notice = %q", gateway.LastReply())
	}
	now = start.Add(5*time.Minute + 30*time.Second + time.Nanosecond)
	notify()
	if !strings.Contains(gateway.LastReply(), "后台输出") || strings.Contains(gateway.LastReply(), "有新的输出") {
		t.Fatalf("inactive notification should contain full console: %q", gateway.LastReply())
	}
}

func TestRouterCurrentOutputRefreshesSharedUserActivity(t *testing.T) {
	router, gateway, relay, home, office := directedRouterHarness(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	router.now = func() time.Time { return now }
	relay.fetchReply = terminalSnapshotResult(home.Ref, hprp.OutputModeText, "当前会话完整输出", 1, 1)
	if err := router.SendNotification(context.Background(), "user-a", "home-mac", statusNotification(home.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gateway.LastReply(), "当前会话完整输出") {
		t.Fatalf("current notification = %q", gateway.LastReply())
	}

	now = now.Add(time.Minute)
	if err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(office.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "有新的输出") || strings.Contains(reply, "后台完整输出") {
		t.Fatalf("background notification = %q", reply)
	}
}

func TestRouterExecutePushStaysFullAndRefreshesActivity(t *testing.T) {
	router, gateway, _, home, office := directedRouterHarness(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	router.now = func() time.Time { return now }
	if err := router.SendCommandOutput(context.Background(), "user-a", routerTerminalCommandOutput(home.Ref, "必须保留的后续分段", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gateway.LastReply(), "必须保留的后续分段") {
		t.Fatalf("push reply = %q", gateway.LastReply())
	}

	now = now.Add(time.Minute)
	if err := router.SendNotification(context.Background(), "user-a", "office-pc", statusNotification(office.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if reply := gateway.LastReply(); !strings.Contains(reply, "有新的输出") || strings.Contains(reply, "不应发送的后台完整输出") {
		t.Fatalf("background notification = %q", reply)
	}
}

func newRouterHarness(t *testing.T) (*ConversationRouter, *routerGateway, *routerRelay) {
	return newRouterHarnessWithConfig(t, ConversationRouterConfig{})
}

func newRouterHarnessWithConfig(t *testing.T, config ConversationRouterConfig) (*ConversationRouter, *routerGateway, *routerRelay) {
	t.Helper()
	deduper, err := policy.NewDeduper(time.Hour, 100, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &routerGateway{}
	text := hprp.Content{Type: hprp.ContentTypeText, Text: "客户端已处理。"}
	relay := &routerRelay{
		executeReply: RelayExecution{StructuredContent: &text},
		capabilities: make(map[string]bool),
	}
	router, err := NewConversationRouterWithConfig(config, NewSessionCatalog(), NewUserExecutor(64), gateway, relay, deduper, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return router, gateway, relay
}

func selectedRouterHarness(t *testing.T) (*ConversationRouter, *routerGateway, *routerRelay) {
	t.Helper()
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	entry := router.catalog.CreateNumberedSnapshot("user-a")[0]
	if err := router.catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	return router, gateway, relay
}

func directedRouterHarness(t *testing.T) (*ConversationRouter, *routerGateway, *routerRelay, CatalogEntry, CatalogEntry) {
	t.Helper()
	router, gateway, relay := newRouterHarness(t)
	attachSnapshot(t, router.catalog, "conn-home", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(1, "pane-home", "occ-home", "home")},
	})
	attachSnapshot(t, router.catalog, "conn-office", ClientKey{UserID: "user-a", MachineID: "office-pc"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{relaySession(2, "pane-office", "occ-office", "office")},
	})
	entries := router.catalog.CreateNumberedSnapshot("user-a")
	if len(entries) != 2 || entries[0].Ref.MachineID != "home-mac" || entries[1].Ref.MachineID != "office-pc" {
		t.Fatalf("numbered entries = %#v", entries)
	}
	if err := router.catalog.SetSelection("user-a", entries[0].Ref); err != nil {
		t.Fatal(err)
	}
	return router, gateway, relay, entries[0], entries[1]
}

func routerMessage(requestID, messageID, userID, content string) im.IncomingText {
	return im.IncomingText{RequestID: requestID, MessageID: messageID, UserID: userID, ChatType: "single", Content: content}
}

func routerCommandOutput(target hprp.Target, content string) hprp.CommandOutput {
	return hprp.CommandOutput{
		Target: target, Sequence: 1,
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: content},
	}
}

func routerTerminalCommandOutput(target hprp.Target, content string, current, total int) hprp.CommandOutput {
	return hprp.CommandOutput{
		Target: target, Sequence: 1,
		Content: hprp.Content{
			Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeText, Text: content,
			Page: &hprp.TerminalPage{Current: current, Total: total},
		},
	}
}

func statusNotification(target hprp.Target, previous, current string) hprp.NotificationEvent {
	return hprp.NotificationEvent{
		EventKey: "event-" + target.MachineID + "-" + target.SlotID,
		Sequence: 1, Kind: hprp.NotificationKindAgentStatusChanged, Target: target,
		SnapshotSequence: 1, OccurredAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Data: &hprp.StatusChangeData{PreviousStatus: previous, Status: current},
	}
}

func invalidatedNotification(target hprp.Target) hprp.NotificationEvent {
	return hprp.NotificationEvent{
		EventKey: "invalidated-" + target.MachineID + "-" + target.SlotID,
		Sequence: 1, Kind: hprp.NotificationKindTargetInvalidated, Target: target,
		SnapshotSequence: 2, OccurredAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
}

func terminalSnapshotResult(target hprp.Target, mode hprp.OutputMode, content string, current, total int) hprp.TerminalSnapshotResult {
	terminal := hprp.Content{
		Type: hprp.ContentTypeTerminal, Mode: mode, Text: content,
		Page: &hprp.TerminalPage{Current: current, Total: total},
	}
	return hprp.TerminalSnapshotResult{Outcome: hprp.OutcomeOK, Target: target, Content: &terminal}
}

func terminalImageContent(content string, current, total int) hprp.Content {
	return hprp.Content{
		Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeImage, Text: content,
		Page: &hprp.TerminalPage{Current: current, Total: total},
		Image: &hprp.TerminalImage{
			MediaType: "image/png", Encoding: "base64", Data: base64.StdEncoding.EncodeToString(testPNG),
			Width: 1, Height: 1, ColorMode: "indexed-256",
		},
	}
}

var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x03, 0x00, 0x00, 0x00, 0x28, 0xcb, 0x34,
	0xbb, 0x00, 0x00, 0x00, 0x03, 0x50, 0x4c, 0x54,
	0x45, 0x00, 0x00, 0x00, 0xa7, 0x7a, 0x3d, 0xda,
	0x00, 0x00, 0x00, 0x01, 0x74, 0x52, 0x4e, 0x53,
	0x00, 0x40, 0xe6, 0xd8, 0x66, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63,
	0x60, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, 0xe2,
	0x21, 0xbc, 0x33, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type routerGateway struct {
	mu      sync.Mutex
	replies []string
	images  [][]byte
}

type routerRegistrationRequester struct {
	result    machinereg.RegisterResult
	err       error
	token     string
	calls     int
	lastInput machinereg.RegisterInput
}

func (requester *routerRegistrationRequester) Register(ctx context.Context, input machinereg.RegisterInput, deliver machinereg.KeyDeliveryFunc) (machinereg.RegisterResult, error) {
	requester.calls++
	requester.lastInput = input
	if requester.err != nil {
		return machinereg.RegisterResult{}, requester.err
	}
	if requester.result.Disposition == machinereg.DispositionAutoIssued {
		if err := deliver(ctx, machinereg.KeyDelivery{
			PrincipalID: input.PrincipalID, MachineID: input.MachineID,
			CredentialID: requester.result.CredentialID, Token: requester.token,
		}); err != nil {
			return machinereg.RegisterResult{}, machinereg.ErrDeliveryFailed
		}
	}
	return requester.result, nil
}

type routerAuditCollector struct {
	mu     sync.Mutex
	events []audit.Event
}

func (collector *routerAuditCollector) Emit(event audit.Event) {
	collector.mu.Lock()
	collector.events = append(collector.events, event)
	collector.mu.Unlock()
}

func (collector *routerAuditCollector) Shutdown(context.Context) error { return nil }

func (collector *routerAuditCollector) Events() []audit.Event {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]audit.Event(nil), collector.events...)
}

type failingRouterGateway struct {
	err error
}

type registrationFailingGateway struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (gateway *registrationFailingGateway) RespondMarkdown(context.Context, string, string) error {
	gateway.mu.Lock()
	gateway.calls++
	gateway.mu.Unlock()
	return gateway.err
}

func (gateway *registrationFailingGateway) SendMarkdownTo(context.Context, string, string) error {
	return gateway.err
}

func (gateway *registrationFailingGateway) SendImageTo(context.Context, string, []byte) error {
	return gateway.err
}

func (gateway *registrationFailingGateway) CallCount() int {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.calls
}

type failingImageHeaderGateway struct {
	err        error
	imageCalls int
}

type imageFallbackGateway struct{}

func (*imageFallbackGateway) RespondMarkdown(context.Context, string, string) error { return nil }
func (*imageFallbackGateway) SendMarkdownTo(context.Context, string, string) error  { return nil }
func (*imageFallbackGateway) SendImageTo(context.Context, string, []byte) error {
	return errors.New("image upload failed")
}

func (gateway *failingImageHeaderGateway) RespondMarkdown(context.Context, string, string) error {
	return gateway.err
}

func (gateway *failingImageHeaderGateway) SendMarkdownTo(context.Context, string, string) error {
	return nil
}

func (gateway *failingImageHeaderGateway) SendImageTo(context.Context, string, []byte) error {
	gateway.imageCalls++
	return nil
}

type blockingImageGateway struct {
	mu           sync.Mutex
	events       []string
	imageStarted chan struct{}
	releaseImage chan struct{}
}

func newBlockingImageGateway() *blockingImageGateway {
	return &blockingImageGateway{imageStarted: make(chan struct{}), releaseImage: make(chan struct{})}
}

func (gateway *blockingImageGateway) RespondMarkdown(ctx context.Context, _ string, content string) error {
	return gateway.SendMarkdownTo(ctx, "user-a", content)
}

func (gateway *blockingImageGateway) SendMarkdownTo(_ context.Context, _ string, content string) error {
	event := "text"
	if strings.HasPrefix(content, "[终端输出#") {
		event = "header"
	}
	gateway.mu.Lock()
	gateway.events = append(gateway.events, event)
	gateway.mu.Unlock()
	return nil
}

func (gateway *blockingImageGateway) SendImageTo(ctx context.Context, _ string, _ []byte) error {
	gateway.mu.Lock()
	gateway.events = append(gateway.events, "image-start")
	gateway.mu.Unlock()
	close(gateway.imageStarted)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gateway.releaseImage:
	}
	gateway.mu.Lock()
	gateway.events = append(gateway.events, "image-end")
	gateway.mu.Unlock()
	return nil
}

func (gateway *blockingImageGateway) Events() []string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return append([]string(nil), gateway.events...)
}

func (gateway failingRouterGateway) RespondMarkdown(context.Context, string, string) error {
	return gateway.err
}

func (gateway failingRouterGateway) SendMarkdownTo(context.Context, string, string) error {
	return gateway.err
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

func (gateway *routerGateway) SendImageTo(_ context.Context, _ string, png []byte) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.images = append(gateway.images, append([]byte(nil), png...))
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

func (gateway *routerGateway) Images() [][]byte {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	result := make([][]byte, len(gateway.images))
	for index := range gateway.images {
		result[index] = append([]byte(nil), gateway.images[index]...)
	}
	return result
}

type routerRelayCall struct {
	kind    string
	userID  string
	target  hprp.Target
	message im.IncomingText
}

type routerFetchCall struct {
	userID   string
	target   hprp.Target
	mode     hprp.OutputMode
	maxLines int
}

type routerRelay struct {
	mu           sync.Mutex
	calls        []routerRelayCall
	fetchCalls   []routerFetchCall
	selectErr    error
	executeReply RelayExecution
	execute      func(context.Context, string, hprp.Target, im.IncomingText) (RelayExecution, error)
	capabilities map[string]bool
	fetchReply   hprp.TerminalSnapshotResult
	fetchErr     error
}

func (relay *routerRelay) Select(_ context.Context, userID string, target hprp.Target) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.calls = append(relay.calls, routerRelayCall{kind: "select", userID: userID, target: target})
	return relay.selectErr
}

func (relay *routerRelay) Execute(ctx context.Context, userID string, target hprp.Target, message im.IncomingText) (RelayExecution, error) {
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

func (relay *routerRelay) SupportsCapability(_ string, _ hprp.Target, capability string) bool {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.capabilities[capability]
}

func (relay *routerRelay) FetchTerminalSnapshot(_ context.Context, userID string, target hprp.Target, mode hprp.OutputMode, maxLines int) (hprp.TerminalSnapshotResult, error) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.fetchCalls = append(relay.fetchCalls, routerFetchCall{userID: userID, target: target, mode: mode, maxLines: maxLines})
	return relay.fetchReply, relay.fetchErr
}

func (relay *routerRelay) Calls() []routerRelayCall {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return append([]routerRelayCall(nil), relay.calls...)
}

func (relay *routerRelay) CallCount() int { return len(relay.Calls()) }

func (relay *routerRelay) FetchCalls() []routerFetchCall {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return append([]routerFetchCall(nil), relay.fetchCalls...)
}

type testDiscardWriter struct{}

func (testDiscardWriter) Write(data []byte) (int, error) { return len(data), nil }

type routerHelpProvider struct {
	text string
	err  error
}

func (provider *routerHelpProvider) Read() (string, error) {
	return provider.text, provider.err
}

type lockedLogBuffer = lockedHPRPLogBuffer
