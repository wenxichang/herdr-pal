package relayclient

import (
	"context"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/session"
	"testing"
)

type muxSink struct{}

func (muxSink) RespondMarkdown(context.Context, string, string) error { return nil }
func (muxSink) SendMarkdown(context.Context, string) error            { return nil }

func TestMuxRoutesByExactTarget(t *testing.T) {
	first := &fakeExecutor{targets: []session.Target{{PaneID: "herdr", OccupantKey: "h"}}}
	second := &fakeExecutor{targets: []session.Target{{PaneID: "dsh:one", OccupantKey: "d"}}}
	first.sink = muxSink{}
	second.sink = muxSink{}
	mux := NewExecutorMux(first, second)
	if len(mux.CurrentTargets()) != 2 {
		t.Fatal("missing backend")
	}
	if err := mux.SelectTarget("dsh:one", "d"); err != nil {
		t.Fatal(err)
	}
	if target, err := mux.SelectedTarget(); err != nil || target.PaneID != "dsh:one" {
		t.Fatalf("target=%v err=%v", target, err)
	}
	if err := mux.SelectTarget("dsh:one", "stale"); err == nil {
		t.Fatal("stale identity accepted")
	}
	mux.HandleMessage(context.Background(), im.IncomingText{Content: "dsh"})
	if first.HandleCount() != 0 || second.HandleCount() != 1 {
		t.Fatal("message crossed backend")
	}
	if err := mux.SelectTarget("herdr", "h"); err != nil {
		t.Fatal(err)
	}
	mux.HandleMessage(context.Background(), im.IncomingText{Content: "herdr"})
	if first.HandleCount() != 1 || second.HandleCount() != 1 {
		t.Fatal("selection did not switch backend")
	}
}

// createdTargetExecutor 在 fakeExecutor 基础上实现可选的 NewSession 目标领取。
type createdTargetExecutor struct {
	fakeExecutor
	created session.Target
	ok      bool
}

func (executor *createdTargetExecutor) TakeCreatedTarget() (session.Target, bool) {
	target, ok := executor.created, executor.ok
	executor.created, executor.ok = session.Target{}, false
	return target, ok
}

func TestMuxTakeCreatedTargetDelegatesToSelectedBackend(t *testing.T) {
	herdr := &fakeExecutor{targets: []session.Target{{PaneID: "herdr", OccupantKey: "h"}}}
	dsh := &createdTargetExecutor{
		fakeExecutor: fakeExecutor{targets: []session.Target{{PaneID: "dsh:one", OccupantKey: "d"}}},
		created:      session.Target{PaneID: "dsh:created", OccupantKey: "c"},
		ok:           true,
	}
	mux := NewExecutorMux(herdr, dsh)

	if _, ok := mux.TakeCreatedTarget(); ok {
		t.Fatal("未选择后端时不应返回 created target")
	}
	if err := mux.SelectTarget("dsh:one", "d"); err != nil {
		t.Fatal(err)
	}
	target, ok := mux.TakeCreatedTarget()
	if !ok || target.PaneID != "dsh:created" {
		t.Fatalf("TakeCreatedTarget() = (%#v, %v), want dsh:created", target, ok)
	}
	if _, ok := mux.TakeCreatedTarget(); ok {
		t.Fatal("created target 必须被领取后清除")
	}

	if err := mux.SelectTarget("herdr", "h"); err != nil {
		t.Fatal(err)
	}
	if _, ok := mux.TakeCreatedTarget(); ok {
		t.Fatal("未实现可选接口的后端必须返回 false")
	}
}
