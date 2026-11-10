package dshlegacy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/policy"
)

type fakeAPI struct {
	sessions []Session
	err      error
	prompts  int
	text     string
}

func (f *fakeAPI) Sessions(context.Context) ([]Session, error) { return f.sessions, f.err }
func (f *fakeAPI) Prompt(_ context.Context, _ Session, _, text string) error {
	f.prompts++
	f.text = text
	return f.err
}
func (f *fakeAPI) Read(context.Context, Session) (string, error) { return "助手：完成", f.err }

type fakeSink struct {
	replies []string
	events  []im.NotificationEvent
}

func (s *fakeSink) RespondMarkdown(_ context.Context, _, text string) error {
	s.replies = append(s.replies, text)
	return nil
}
func (s *fakeSink) SendNotification(_ context.Context, _ im.NotificationTarget, e im.NotificationEvent) error {
	s.events = append(s.events, e)
	return nil
}

func TestExecutorSelectionPolicyAndRecovery(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPI{sessions: []Session{{ID: "one", Incarnation: "first", Status: "idle", Revision: "1"}}}
	sink := &fakeSink{}
	guard, _ := policy.NewGuard("7")
	e := NewExecutor(api, sink, guard)
	e.refresh(ctx)
	targets := e.CurrentTargets()
	if len(targets) != 1 || targets[0].Agent != "dsh" {
		t.Fatal(targets)
	}
	target := targets[0]
	if err := e.SelectTarget(target.PaneID, target.OccupantKey); err != nil {
		t.Fatal(err)
	}
	message := im.IncomingText{UserID: "7", ChatType: "single", RequestID: "r", MessageID: "m", Content: "hello"}
	e.HandleMessage(ctx, message)
	if api.prompts != 1 || api.text != "hello" {
		t.Fatal("prompt not sent")
	}
	message.Content = "/enter"
	e.HandleMessage(ctx, message)
	if api.prompts != 1 {
		t.Fatal("key became prompt")
	}
	message.Content = "hello"
	message.UserID = "attacker"
	e.HandleMessage(ctx, message)
	if api.prompts != 1 {
		t.Fatal("identity bypass")
	}
	api.sessions[0].Revision = "2"
	e.refresh(ctx)
	if len(sink.events) != 1 || sink.events[0].Status != "done" {
		t.Fatal(sink.events)
	}
	e.refresh(ctx)
	if len(sink.events) != 1 {
		t.Fatal("duplicate notification")
	}
	api.sessions[0].Incarnation = "second"
	e.refresh(ctx)
	if _, err := e.SelectedTarget(); err == nil {
		t.Fatal("stale selection retained")
	}
	if err := e.SelectTarget(target.PaneID, target.OccupantKey); err == nil {
		t.Fatal("stale selection accepted")
	}
	api.err = errors.New("offline")
	e.refresh(ctx)
	if len(e.CurrentTargets()) != 0 {
		t.Fatal("offline catalog retained")
	}
	api.err = nil
	e.refresh(ctx)
	if len(e.CurrentTargets()) != 1 {
		t.Fatal("recovery failed")
	}
}

func TestExecutorReadsTextAndRefusesBusy(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPI{sessions: []Session{{ID: "s", Incarnation: "i", Status: "working"}}}
	sink := &fakeSink{}
	guard, _ := policy.NewGuard("7")
	e := NewExecutor(api, sink, guard)
	e.refresh(ctx)
	target := e.CurrentTargets()[0]
	e.SelectTarget(target.PaneID, target.OccupantKey)
	e.HandleMessage(ctx, im.IncomingText{UserID: "7", ChatType: "single", RequestID: "r", MessageID: "m", Content: "hello"})
	if api.prompts != 0 {
		t.Fatal("busy prompt sent")
	}
	content, err := e.ReadTerminalSnapshot(ctx, target.PaneID, target.OccupantKey, im.OutputModeText, 100)
	if err != nil || !strings.Contains(content.Text, "完成") {
		t.Fatalf("content=%v err=%v", content, err)
	}
}
