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
