package bridge

import (
	"context"
	"errors"
	"fmt"
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
			if strings.Contains(fakeIMFromService(t, service).lastReply(), "确认") {
				t.Fatalf("key reply asks confirmation: %q", fakeIMFromService(t, service).lastReply())
			}
		})
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
	mu          sync.Mutex
	agent       herdr.AgentInfo
	getErr      error
	read        herdr.ReadResult
	readErr     error
	promptErr   error
	keyErr      error
	getCalls    []string
	readCalls   []readCall
	promptCalls []promptCall
	keyCalls    []keyCall
	order       []string
}

func (f *fakeHerdr) GetAgent(_ context.Context, target string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, target)
	f.order = append(f.order, "get")
	return f.agent, f.getErr
}
func (f *fakeHerdr) ReadRecent(_ context.Context, target string, lines int) (herdr.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls = append(f.readCalls, readCall{target, lines})
	f.order = append(f.order, "read")
	return f.read, f.readErr
}
func (f *fakeHerdr) Prompt(_ context.Context, target, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promptCalls = append(f.promptCalls, promptCall{target, text})
	f.order = append(f.order, "prompt")
	return f.promptErr
}
func (f *fakeHerdr) SendKey(_ context.Context, target, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyCalls = append(f.keyCalls, keyCall{target, key})
	f.order = append(f.order, "key")
	return f.keyErr
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
	mu         sync.Mutex
	replies    []string
	pushes     []string
	respondErr error
	sendErr    error
}

func (f *fakeIM) RespondMarkdown(_ context.Context, _ string, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, content)
	return f.respondErr
}
func (f *fakeIM) SendMarkdown(_ context.Context, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, content)
	return f.sendErr
}
func (f *fakeIM) replyCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.replies) }
func (f *fakeIM) pushCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pushes) }
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
