package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

func TestHerdrServerCloseTerminatesActiveSubscriptions(t *testing.T) {
	server := NewHerdrServer(t, testkitSnapshot())
	client := herdr.NewClient(server.SocketPath(), nil, time.Second)
	stream, err := client.Subscribe(context.Background(), herdr.LifecycleSubscriptions())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	server.WaitSubscription(t, herdr.LifecycleSubscriptions())

	result := make(chan error, 1)
	go func() {
		_, err := stream.Recv(context.Background())
		result <- err
	}()
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Recv() error = nil, want closed subscription")
		}
	case <-time.After(200 * time.Millisecond):
		_ = stream.Close()
		t.Fatal("Close() 未终止活动订阅")
	}
}

func TestHerdrServerEmitsDecodablePaneStatusEvent(t *testing.T) {
	server := NewHerdrServer(t, testkitSnapshot())
	client := herdr.NewClient(server.SocketPath(), nil, time.Second)
	stream, err := client.Subscribe(context.Background(), herdr.StatusSubscriptions([]string{"pane-1"}))
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer stream.Close()
	server.WaitSubscription(t, herdr.StatusSubscriptions([]string{"pane-1"}))
	agent := "codex"
	if delivered := server.EmitStatus(herdr.AgentStatusEvent{
		PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusBlocked, Agent: &agent,
	}); delivered != 1 {
		t.Fatalf("EmitStatus() deliveries = %d, want 1", delivered)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	decoded, err := herdr.DecodeAgentStatusEvent(event)
	if err != nil {
		t.Fatalf("DecodeAgentStatusEvent() error = %v", err)
	}
	if decoded.PaneID != "pane-1" || decoded.AgentStatus != herdr.AgentStatusBlocked || decoded.Agent == nil || *decoded.Agent != agent {
		t.Fatalf("decoded event = %#v", decoded)
	}
}

func testkitSnapshot() herdr.Snapshot {
	agent := "codex"
	return herdr.Snapshot{
		Version: "test", Protocol: herdr.RequiredProtocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: "workspace-1", Number: 1, Label: "workspace"}},
		Tabs:       []herdr.Tab{{TabID: "tab-1", WorkspaceID: "workspace-1", Number: 1, Label: "tab"}},
		Panes: []herdr.Pane{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, AgentStatus: herdr.AgentStatusWorking,
		}},
		Agents: []herdr.AgentInfo{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, AgentStatus: herdr.AgentStatusWorking,
		}},
	}
}
