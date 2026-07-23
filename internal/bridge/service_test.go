package bridge

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestServiceRejectsUnauthorizedAndGroupBeforeParsing(t *testing.T) {
	service, fake := newTestService(t)
	for _, message := range []wecom.IncomingText{
		{RequestID: "request-user", MessageID: "message-user", UserID: "other", ChatType: "single", Content: "/unknown secret"},
		{RequestID: "request-group", MessageID: "message-group", UserID: "user-1", ChatType: "group", Content: "/unknown secret"},
	} {
		service.HandleMessage(context.Background(), message)
	}
	if fake.callCount() != 0 {
		t.Fatalf("unauthorized input called Herdr: %#v", fake)
	}
	if got := fakeIMFromService(t, service).replyCount(); got != 0 {
		t.Fatalf("unauthorized input replied %d times", got)
	}
}

func TestServiceDeduplicatesAndRejectsEmptyMessageID(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	message := incoming("same-id", "保留原文  \n")
	service.HandleMessage(context.Background(), message)
	service.HandleMessage(context.Background(), message)
	if got := len(fake.prompts()); got != 1 {
		t.Fatalf("prompt calls = %d, want 1", got)
	}
	service.HandleMessage(context.Background(), incoming("", "ignored"))
	if got := len(fake.prompts()); got != 1 {
		t.Fatalf("empty message id sent prompt: %#v", fake.prompts())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "消息标识") {
		t.Fatalf("empty message ID reply = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceUnknownCommandShowsUsageWithoutHerdr(t *testing.T) {
	service, fake := newTestService(t)
	service.HandleMessage(context.Background(), incoming("unknown", "/unknown secret-prompt"))
	if fake.callCount() != 0 {
		t.Fatalf("unknown command called Herdr: %#v", fake)
	}
	got := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(got, "用法") || strings.Contains(got, "secret-prompt") {
		t.Fatalf("usage reply = %q", got)
	}
}

func TestServiceDegradedAllowsListButPausesHerdrActions(t *testing.T) {
	service, fake := newTestService(t)
	service.SetHerdr(nil)
	for index, content := range []string{"/ls", "/con", "/key enter", "prompt"} {
		service.HandleMessage(context.Background(), incoming(fmt.Sprintf("degraded-%d", index), content))
	}
	if fake.callCount() != 0 {
		t.Fatalf("degraded service called Herdr: %#v", fake)
	}
	im := fakeIMFromService(t, service)
	if !strings.Contains(im.replies[0], "Agent") || !strings.Contains(im.lastReply(), "暂不可用") {
		t.Fatalf("degraded replies = %#v", im.replies)
	}
}

func TestServiceListAndSelectionResetPanel(t *testing.T) {
	service, _ := newTestService(t)
	service.HandleMessage(context.Background(), incoming("list", "/ls"))
	list := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(list, "1.") || !strings.Contains(list, "workspace-1") || strings.Contains(list, "当前选择") {
		t.Fatalf("list reply = %q", list)
	}
	service.HandleMessage(context.Background(), incoming("select", "/sel 1"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "已选择") {
		t.Fatalf("select reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("list-selected", "/ls"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "当前选择") {
		t.Fatalf("selected list = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceListIncludesStableHierarchyTitleAndCurrentSelection(t *testing.T) {
	service, _ := newTestService(t)
	service.registry.Replace(herdr.Snapshot{
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-2", Number: 2, Label: "工作区二"}, {WorkspaceID: "workspace-1", Number: 1, Label: "工作区一"}},
		Tabs:       []herdr.Tab{{TabID: "tab-2", WorkspaceID: "workspace-2", Number: 2, Label: "标签二"}, {TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "标签一"}},
		Panes: []herdr.Pane{
			{PaneID: "pane-z", TerminalID: "terminal-z", WorkspaceID: "workspace-2", TabID: "tab-2", Agent: stringRef("codex"), DisplayAgent: stringRef("Codex"), Title: stringRef("第二项"), AgentStatus: herdr.AgentStatusDone},
			{PaneID: "pane-a", TerminalID: "terminal-a", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), Title: stringRef("第一项\n```注入"), AgentStatus: herdr.AgentStatusBlocked},
		},
	}, false)
	service.HandleMessage(context.Background(), incoming("list-details", "/ls"))
	list := fakeIMFromService(t, service).lastReply()
	for _, want := range []string{"1. Claude", "标题：第一项 ``\u200b`注入", "工作区一 / 标签一", "状态：blocked", "面板：pane-a", "2. Codex", "标题：第二项", "工作区二 / 标签二", "状态：done", "面板：pane-z"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list missing %q: %q", want, list)
		}
	}
	service.HandleMessage(context.Background(), incoming("list-select", "/sel 1"))
	service.HandleMessage(context.Background(), incoming("list-current", "/ls"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "1. Claude（当前选择）") {
		t.Fatalf("current marker missing: %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServicePromptPreservesBytesAndChecksLiveAgent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	prompt := "  keep\nall bytes  "
	service.HandleMessage(context.Background(), incoming("prompt", prompt))
	if got := fake.prompts(); len(got) != 1 || got[0].text != prompt || got[0].target != "terminal-1" {
		t.Fatalf("prompt calls = %#v", got)
	}
	if got := fake.callOrder(); strings.Join(got, ",") != "get,prompt" {
		t.Fatalf("call order = %#v, want get,prompt", got)
	}
	reply := fakeIMFromService(t, service).lastReply()
	if !strings.Contains(reply, "已发送") || strings.Contains(reply, "keep") {
		t.Fatalf("prompt reply = %q", reply)
	}
}

func TestServiceKeyAliasesSendOnceWithoutConfirmation(t *testing.T) {
	for _, test := range []struct {
		content string
		key     string
	}{
		{"/keyup", "up"}, {"/key up", "up"}, {"/keydn", "down"}, {"/key down", "down"},
		{"/enter", "enter"}, {"/key enter", "enter"}, {"/esc", "esc"}, {"/key esc", "esc"},
		{"/space", "space"}, {"/key space", "space"}, {"/key A", "A"},
	} {
		t.Run(test.content, func(t *testing.T) {
			service, fake := newTestService(t)
			selectTarget(t, service)
			service.HandleMessage(context.Background(), incoming("key-"+test.key, test.content))
			if got := fake.keys(); len(got) != 1 || got[0].key != test.key {
				t.Fatalf("key calls = %#v", got)
			}
			if got := strings.Join(fake.callOrder(), ","); got != "get,key" {
				t.Fatalf("key call order = %q, want get,key", got)
			}
			if strings.Contains(fakeIMFromService(t, service).lastReply(), "确认") {
				t.Fatalf("key reply asks confirmation: %q", fakeIMFromService(t, service).lastReply())
			}
		})
	}
}

func TestServiceKeyMismatchClearsSelectionWithoutSending(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.setAgent(agentInfo("pane-1", "terminal-1", "other"))
	service.HandleMessage(context.Background(), incoming("key-mismatch", "/key enter"))
	if len(fake.keys()) != 0 || !strings.Contains(fakeIMFromService(t, service).lastReply(), "/sel") {
		t.Fatalf("mismatch keys=%#v reply=%q", fake.keys(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceLiveMismatchClearsSelectionWithoutInput(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.agent = agentInfo("pane-1", "terminal-1", "other")
	service.HandleMessage(context.Background(), incoming("mismatch", "prompt"))
	if len(fake.prompts()) != 0 {
		t.Fatalf("mismatch sent prompt: %#v", fake.prompts())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/ls") {
		t.Fatalf("mismatch reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("mismatch-next", "again"))
	if len(fake.gets()) != 1 {
		t.Fatalf("cleared selection still queried agent: %#v", fake.gets())
	}
}

func TestServiceContentAndPaging(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	if got := fake.reads(); len(got) != 1 || got[0].lines != 100 {
		t.Fatalf("content reads = %#v", got)
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "第 0 页") {
		t.Fatalf("content reply = %q", fakeIMFromService(t, service).lastReply())
	}
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("pageup", "/pageup"))
	if got := fake.reads(); len(got) != 2 || got[1].lines != 200 {
		t.Fatalf("pageup reads = %#v", got)
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "第 1 页") {
		t.Fatalf("pageup reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("pagedown", "/pagedn"))
	if got := len(fake.reads()); got != 2 {
		t.Fatalf("pagedown unexpectedly read Herdr: %#v", fake.reads())
	}
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "第 0 页") {
		t.Fatalf("pagedown reply = %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceSelectResetsExistingPanelCache(t *testing.T) {
	service, fake := newTestService(t)
	service.registry.Replace(twoTargetSnapshot(), false)
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("select-content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("select-pageup", "/pageup"))
	service.HandleMessage(context.Background(), incoming("select-list-again", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-two", "/sel 2"))
	beforeReads := len(fake.reads())
	service.HandleMessage(context.Background(), incoming("select-pagedown", "/pagedn"))
	if len(fake.reads()) != beforeReads || !strings.Contains(fakeIMFromService(t, service).lastReply(), "/con") {
		t.Fatalf("selection retained old panel: reads=%#v reply=%q", fake.reads(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceBlockedReadCannotRestoreOldPanelAfterSelectionChanges(t *testing.T) {
	service, fake := newTestService(t)
	service.registry.Replace(twoTargetSnapshot(), false)
	service.HandleMessage(context.Background(), incoming("read-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("read-select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "OLD-TERMINAL-CONTENT"}
	fake.blockRead = make(chan struct{})
	fake.readStarted = make(chan struct{}, 1)
	readDone := make(chan struct{})
	go func() { service.HandleMessage(context.Background(), incoming("read-old", "/con")); close(readDone) }()
	awaitSignal(t, fake.readStarted, "ReadRecent")
	service.HandleMessage(context.Background(), incoming("read-list-again", "/ls"))
	service.HandleMessage(context.Background(), incoming("read-select-two", "/sel 2"))
	close(fake.blockRead)
	awaitSignal(t, readDone, "old /con")
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-TERMINAL-CONTENT") || !strings.Contains(reply, "/con") {
		t.Fatalf("old read was applied after selection switch: %q", reply)
	}
	service.HandleMessage(context.Background(), incoming("read-after-pagedown", "/pagedn"))
	if reply := fakeIMFromService(t, service).lastReply(); strings.Contains(reply, "OLD-TERMINAL-CONTENT") || !strings.Contains(reply, "/con") {
		t.Fatalf("old panel leaked after selection switch: %q", reply)
	}
}

func TestServiceContentNormalizesTerminalControls(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "\x1b]0;title\a\x1b[31mred\x1b[0m\rfinal\x00  \n"}
	service.HandleMessage(context.Background(), incoming("normalize", "/con"))
	reply := fakeIMFromService(t, service).lastReply()
	if strings.Contains(reply, "\x1b") || strings.Contains(reply, "red") || !strings.Contains(reply, "final") {
		t.Fatalf("terminal normalization reply = %q", reply)
	}
}

func TestServicePageUpExpandsReadsUntilMaximum(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(900, 1000)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	for page := 1; page < 10; page++ {
		fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(1000-(page+1)*100, 1000)}
		service.HandleMessage(context.Background(), incoming(fmt.Sprintf("pageup-%d", page), "/pageup"))
	}
	reads := fake.reads()
	if len(reads) != 10 {
		t.Fatalf("read count = %d, want 10", len(reads))
	}
	for index, call := range reads {
		want := (index + 1) * 100
		if call.lines != want {
			t.Fatalf("read %d lines = %d, want %d", index, call.lines, want)
		}
	}
	service.HandleMessage(context.Background(), incoming("pageup-oldest", "/pageup"))
	if len(fake.reads()) != 10 || !strings.Contains(fakeIMFromService(t, service).lastReply(), "最早") {
		t.Fatalf("oldest page behaviour reads=%#v reply=%q", fake.reads(), fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceReadPaneMismatchInvalidatesSelection(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "other-pane", Text: "content"}
	service.HandleMessage(context.Background(), incoming("wrong-pane", "/con"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/ls") {
		t.Fatalf("read mismatch reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("wrong-pane-next", "prompt"))
	if len(fake.gets()) != 0 {
		t.Fatalf("read mismatch retained selection: %#v", fake.gets())
	}
}

func TestNewServiceRejectsMissingDependencies(t *testing.T) {
	registry := &session.Registry{}
	guard, err := policy.NewGuard("user-1")
	if err != nil {
		t.Fatal(err)
	}
	deduper, err := policy.NewDeduper(time.Minute, 1, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(nil, &panel.Buffer{}, guard, deduper, &fakeIM{}); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil registry error = %v", err)
	}
	if _, err := NewService(registry, nil, guard, deduper, &fakeIM{}); !errors.Is(err, ErrInvalidServiceDependency) {
		t.Fatalf("nil panel error = %v", err)
	}
}

func TestServicePanelChangedResetsSelectionAndCache(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: "different"}
	service.HandleMessage(context.Background(), incoming("pageup", "/pageup"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/con") {
		t.Fatalf("changed panel reply = %q", fakeIMFromService(t, service).lastReply())
	}
	service.HandleMessage(context.Background(), incoming("pagedown", "/pagedn"))
	if !strings.Contains(fakeIMFromService(t, service).lastReply(), "/con") {
		t.Fatalf("page cache remained after change: %q", fakeIMFromService(t, service).lastReply())
	}
}

func TestServiceSplitsTerminalReplyAndStopsAfterIMFailure(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: strings.Repeat("中文终端内容\n", 9000)}
	im := fakeIMFromService(t, service)
	beforeReplies, beforePushes := im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("split", "/con"))
	if im.replyCount() != beforeReplies+1 || im.pushCount() <= beforePushes {
		t.Fatalf("split deliveries replies=%#v pushes=%#v", im.replies, im.pushes)
	}
	for _, content := range append(append([]string(nil), im.replies...), im.pushes...) {
		if len(content) > panel.WeComContentLimit {
			t.Fatalf("content exceeds limit: %d", len(content))
		}
	}

	service, fake = newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: strings.Repeat("line\n", 6000)}
	im = fakeIMFromService(t, service)
	im.respondErr = errors.New("network")
	_, beforePushes = im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("failed-reply", "/con"))
	if im.pushCount() != beforePushes {
		t.Fatalf("sent push after response failure: %#v", im.pushes)
	}
}

func TestServiceStopsAfterFirstPushFailure(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: strings.Repeat("终端行\n", 9000)}
	im := fakeIMFromService(t, service)
	im.sendErr = errors.New("network")
	_, beforePushes := im.deliveryCounts()
	service.HandleMessage(context.Background(), incoming("push-fail", "/con"))
	if got := im.pushCount() - beforePushes; got != 1 {
		t.Fatalf("push attempts = %d, want 1", got)
	}
}

func TestServiceInvalidateDoesNotWaitForBlockingIMReply(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	im := fakeIMFromService(t, service)
	im.blockRespond = make(chan struct{})
	im.respondStarted = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocking-reply", "prompt"))
		close(done)
	}()
	awaitSignal(t, im.respondStarted, "RespondMarkdown")
	invalidated := make(chan struct{})
	go func() { service.InvalidateSelection(); close(invalidated) }()
	awaitSignal(t, invalidated, "InvalidateSelection")
	close(im.blockRespond)
	awaitSignal(t, done, "HandleMessage")
	if len(fake.prompts()) != 1 {
		t.Fatalf("prompt calls = %#v", fake.prompts())
	}
}

func TestServiceSetNilWaitsForHerdrOperationAndThenBlocksNewOperations(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocked-prompt", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")
	setDone := make(chan struct{})
	go func() { service.SetHerdr(nil); close(setDone) }()
	assertNotClosed(t, setDone, "SetHerdr(nil) returned before Prompt")
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "prompt HandleMessage")
	awaitSignal(t, setDone, "SetHerdr(nil)")
	before := fake.callCount()
	service.HandleMessage(context.Background(), incoming("after-nil", "prompt"))
	service.HandleMessage(context.Background(), incoming("after-nil-key", "/key enter"))
	service.HandleMessage(context.Background(), incoming("after-nil-read", "/con"))
	if fake.callCount() != before {
		t.Fatalf("new operation called old client after nil: %#v", fake)
	}
}

func TestServiceConcurrentInvalidationsDoNotLetInputThrough(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	service.InvalidateSelection()
	selectTarget(t, service)
	service.stateMu.Lock()
	secondDone := make(chan struct{})
	go func() { service.InvalidateSelection(); close(secondDone) }()
	waitForCondition(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.inputBlocked > 0
	}, "second invalidation input barrier")
	before := fake.callCount()
	promptDone, keyDone := make(chan struct{}), make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("multi-invalidate-prompt", "prompt"))
		close(promptDone)
	}()
	go func() {
		service.HandleMessage(context.Background(), incoming("multi-invalidate-key", "/key enter"))
		close(keyDone)
	}()
	awaitSignal(t, promptDone, "blocked prompt")
	awaitSignal(t, keyDone, "blocked key")
	if fake.callCount() != before {
		t.Fatalf("input passed through active invalidation: %#v", fake)
	}
	service.stateMu.Unlock()
	awaitSignal(t, secondDone, "second InvalidateSelection")
	service.HandleMessage(context.Background(), incoming("multi-invalidate-after", "prompt"))
	if fake.callCount() != before {
		t.Fatalf("input passed after invalidation completed: %#v", fake)
	}
}

func TestServiceInvalidateAndSetNilWaitForBlockingGetAgent(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	fake.blockGet = make(chan struct{})
	fake.getStarted = make(chan struct{}, 1)
	inputDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("blocked-get", "prompt"))
		close(inputDone)
	}()
	awaitSignal(t, fake.getStarted, "GetAgent")
	invalidated := make(chan struct{})
	setDone := make(chan struct{})
	go func() { service.InvalidateSelection(); close(invalidated) }()
	go func() { service.SetHerdr(nil); close(setDone) }()
	assertNotClosed(t, invalidated, "InvalidateSelection returned before GetAgent")
	assertNotClosed(t, setDone, "SetHerdr(nil) returned before GetAgent")
	close(fake.blockGet)
	awaitSignal(t, inputDone, "input")
	awaitSignal(t, invalidated, "InvalidateSelection")
	awaitSignal(t, setDone, "SetHerdr(nil)")
}

func TestServiceSelectWaitsForInFlightPrompt(t *testing.T) {
	service, fake := newTestService(t)
	service.registry.Replace(twoTargetSnapshot(), false)
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-one", "/sel 1"))
	fake.blockPrompt = make(chan struct{})
	fake.promptStarted = make(chan struct{}, 1)
	promptDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("select-blocked", "prompt"))
		close(promptDone)
	}()
	awaitSignal(t, fake.promptStarted, "Prompt")
	selectDone := make(chan struct{})
	go func() {
		service.HandleMessage(context.Background(), incoming("select-two", "/sel 2"))
		close(selectDone)
	}()
	assertNotClosed(t, selectDone, "/sel returned before Prompt")
	close(fake.blockPrompt)
	awaitSignal(t, promptDone, "prompt")
	awaitSignal(t, selectDone, "/sel")
	fake.setAgent(herdr.AgentInfo{PaneID: "pane-2", TerminalID: "terminal-2", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude")})
	service.HandleMessage(context.Background(), incoming("select-two-prompt", "prompt"))
	got := fake.prompts()
	if len(got) != 2 || got[1].target != "terminal-2" {
		t.Fatalf("selection target calls = %#v", got)
	}
}

func TestServiceConcurrentSelectAndPageDownNeverMixTargetAndPanel(t *testing.T) {
	service, fake := newTestService(t)
	service.registry.Replace(twoTargetSnapshot(), false)
	service.HandleMessage(context.Background(), incoming("mix-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("mix-select-one", "/sel 1"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(100, 200)}
	service.HandleMessage(context.Background(), incoming("mix-content", "/con"))
	fake.read = herdr.ReadResult{PaneID: "pane-1", Text: textLines(0, 200)}
	service.HandleMessage(context.Background(), incoming("mix-pageup", "/pageup"))
	im := fakeIMFromService(t, service)
	beforeReplies := im.replyCount()
	service.stateMu.Lock()
	start := make(chan struct{})
	pageDone, selectDone := make(chan struct{}), make(chan struct{})
	go func() {
		<-start
		service.HandleMessage(context.Background(), incoming("mix-pagedown", "/pagedn"))
		close(pageDone)
	}()
	go func() {
		<-start
		service.HandleMessage(context.Background(), incoming("mix-select-two", "/sel 2"))
		close(selectDone)
	}()
	close(start)
	service.stateMu.Unlock()
	awaitSignal(t, pageDone, "/pagedn")
	awaitSignal(t, selectDone, "/sel")
	for _, reply := range im.repliesFrom(beforeReplies) {
		if strings.Contains(reply, "line-000") && strings.Contains(reply, "Claude（pane-2）") {
			t.Fatalf("new target title was combined with old panel: %q", reply)
		}
	}
}

func TestServiceConcurrentDuplicateAndStateChangesAreRaceSafe(t *testing.T) {
	service, fake := newTestService(t)
	selectTarget(t, service)
	message := incoming("concurrent", "prompt")
	var group sync.WaitGroup
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			service.HandleMessage(context.Background(), message)
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			service.SetHerdr(fake)
			service.InvalidateSelection()
		}()
	}
	group.Wait()
	if got := len(fake.prompts()); got > 1 {
		t.Fatalf("duplicate calls sent %d prompts", got)
	}
}

func newTestService(t *testing.T) (*Service, *fakeHerdr) {
	t.Helper()
	registry := &session.Registry{}
	registry.Replace(testSnapshot(), false)
	guard, err := policy.NewGuard("user-1")
	if err != nil {
		t.Fatal(err)
	}
	deduper, err := policy.NewDeduper(time.Hour, 1000, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeHerdr{agent: agentInfo("pane-1", "terminal-1", "codex")}
	im := &fakeIM{}
	service, err := NewService(registry, &panel.Buffer{}, guard, deduper, im)
	if err != nil {
		t.Fatal(err)
	}
	service.SetHerdr(fake)
	return service, fake
}

func selectTarget(t *testing.T, service *Service) {
	t.Helper()
	service.HandleMessage(context.Background(), incoming("select-list", "/ls"))
	service.HandleMessage(context.Background(), incoming("select-target", "/sel 1"))
}

func incoming(messageID, content string) wecom.IncomingText {
	return wecom.IncomingText{RequestID: "request-" + messageID, MessageID: messageID, UserID: "user-1", ChatType: "single", Content: content}
}

func testSnapshot() herdr.Snapshot {
	return herdr.Snapshot{
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "workspace-1"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "tab-1"}},
		Panes:      []herdr.Pane{{PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("codex"), DisplayAgent: stringRef("Codex"), AgentStatus: herdr.AgentStatusWorking}},
	}
}

func twoTargetSnapshot() herdr.Snapshot {
	snapshot := testSnapshot()
	snapshot.Panes = append(snapshot.Panes, herdr.Pane{PaneID: "pane-2", TerminalID: "terminal-2", WorkspaceID: "workspace-1", TabID: "tab-1", Agent: stringRef("claude"), DisplayAgent: stringRef("Claude"), Title: stringRef("second"), AgentStatus: herdr.AgentStatusIdle})
	return snapshot
}

func agentInfo(paneID, terminalID, agent string) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: paneID, TerminalID: terminalID, Agent: stringRef(agent), DisplayAgent: stringRef("Codex")}
}

func stringRef(value string) *string { return &value }

func textLines(start, end int) string {
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, fmt.Sprintf("line-%03d", index))
	}
	return strings.Join(lines, "\n")
}

type promptCall struct{ target, text string }
type keyCall struct{ target, key string }
type readCall struct {
	target string
	lines  int
}

type fakeHerdr struct {
	mu            sync.Mutex
	agent         herdr.AgentInfo
	getErr        error
	read          herdr.ReadResult
	readErr       error
	promptErr     error
	keyErr        error
	getCalls      []string
	readCalls     []readCall
	promptCalls   []promptCall
	keyCalls      []keyCall
	order         []string
	blockGet      chan struct{}
	blockRead     chan struct{}
	blockPrompt   chan struct{}
	blockKey      chan struct{}
	getStarted    chan struct{}
	readStarted   chan struct{}
	promptStarted chan struct{}
	keyStarted    chan struct{}
}

func (f *fakeHerdr) GetAgent(_ context.Context, target string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, target)
	f.order = append(f.order, "get")
	agent, err, block, started := f.agent, f.getErr, f.blockGet, f.getStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return agent, err
}
func (f *fakeHerdr) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	f.mu.Lock()
	f.readCalls = append(f.readCalls, readCall{target, lines})
	f.order = append(f.order, "read")
	result, err, block, started := f.read, f.readErr, f.blockRead, f.readStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return result, err
}
func (f *fakeHerdr) Prompt(_ context.Context, target, text string) error {
	f.mu.Lock()
	f.promptCalls = append(f.promptCalls, promptCall{target, text})
	f.order = append(f.order, "prompt")
	err, block, started := f.promptErr, f.blockPrompt, f.promptStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return err
}
func (f *fakeHerdr) SendKey(_ context.Context, target, key string) error {
	f.mu.Lock()
	f.keyCalls = append(f.keyCalls, keyCall{target, key})
	f.order = append(f.order, "key")
	err, block, started := f.keyErr, f.blockKey, f.keyStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return err
}
func (f *fakeHerdr) setAgent(agent herdr.AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agent = agent
}
func (f *fakeHerdr) prompts() []promptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]promptCall(nil), f.promptCalls...)
}
func (f *fakeHerdr) keys() []keyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]keyCall(nil), f.keyCalls...)
}
func (f *fakeHerdr) reads() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readCall(nil), f.readCalls...)
}
func (f *fakeHerdr) gets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getCalls...)
}
func (f *fakeHerdr) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}
func (f *fakeHerdr) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.getCalls) + len(f.readCalls) + len(f.promptCalls) + len(f.keyCalls)
}

type fakeIM struct {
	mu             sync.Mutex
	replies        []string
	pushes         []string
	respondErr     error
	sendErr        error
	blockRespond   chan struct{}
	respondStarted chan struct{}
}

func (f *fakeIM) RespondMarkdown(_ context.Context, _ string, content string) error {
	f.mu.Lock()
	f.replies = append(f.replies, content)
	err, block, started := f.respondErr, f.blockRespond, f.respondStarted
	f.mu.Unlock()
	signal(started)
	if block != nil {
		<-block
	}
	return err
}
func (f *fakeIM) SendMarkdown(_ context.Context, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, content)
	return f.sendErr
}
func (f *fakeIM) replyCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.replies) }
func (f *fakeIM) repliesFrom(index int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.replies) {
		return nil
	}
	return append([]string(nil), f.replies[index:]...)
}
func (f *fakeIM) pushCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pushes) }
func (f *fakeIM) deliveryCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies), len(f.pushes)
}
func (f *fakeIM) lastReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) == 0 {
		return ""
	}
	return f.replies[len(f.replies)-1]
}

func fakeIMFromService(t *testing.T, service *Service) *fakeIM {
	t.Helper()
	im, ok := service.im.(*fakeIM)
	if !ok {
		t.Fatalf("service IM = %T, want *fakeIM", service.im)
	}
	return im
}

func signal(channel chan<- struct{}) {
	if channel != nil {
		channel <- struct{}{}
	}
}

func awaitSignal(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal(name)
	case <-time.After(30 * time.Millisecond):
	}
}

func waitForCondition(t *testing.T, predicate func() bool, name string) {
	t.Helper()
	deadline := time.After(time.Second)
	for !predicate() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", name)
		default:
			runtime.Gosched()
		}
	}
}
