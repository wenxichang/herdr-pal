package testkit

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
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

func TestHerdrServerRejectsInvalidPublicRequestShapes(t *testing.T) {
	server := NewHerdrServer(t, testkitSnapshot())
	tests := []struct {
		name   string
		method string
		params any
	}{
		{name: "read source", method: "agent.read", params: map[string]any{"target": "terminal-1", "source": "recent", "lines": 100, "format": "text", "strip_ansi": true}},
		{name: "unknown subscription", method: "events.subscribe", params: map[string]any{"subscriptions": []map[string]any{{"type": "pane.output_changed"}}}},
		{name: "status without pane", method: "events.subscribe", params: map[string]any{"subscriptions": []map[string]any{{"type": "pane.agent_status_changed"}}}},
		{name: "prompt missing text", method: "agent.prompt", params: map[string]any{"target": "terminal-1"}},
		{name: "send keys missing key", method: "agent.send_keys", params: map[string]any{"target": "terminal-1", "keys": []string{}}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := rawHerdrRequest(t, server.SocketPath(), map[string]any{
				"id": "invalid-" + string(rune('a'+index)), "method": test.method, "params": test.params,
			})
			if response.Error == nil || response.Error.Code != "invalid_params" {
				t.Fatalf("response = %#v, want invalid_params", response)
			}
		})
	}
}

type rawHerdrResponse struct {
	ID    string `json:"id"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func rawHerdrRequest(t *testing.T, socketPath string, request any) rawHerdrResponse {
	t.Helper()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	var response rawHerdrResponse
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	return response
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
