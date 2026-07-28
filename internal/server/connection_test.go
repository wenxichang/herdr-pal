package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestClientConnectionCorrelatesReplyToWithPendingRequest(t *testing.T) {
	connection := newClientConnection(context.Background(), "connection-1", "cred-test", ClientKey{UserID: "user-a", MachineID: "home-mac"}, nil, 2, 2, slog.Default())
	connection.ready.Store(true)
	request, err := hprp.NewEnvelope(hprp.TypeCommandExecute, "command-1", "", true, hprp.CommandExecute{
		IdempotencyKey: "message-1",
		Target:         hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"},
		Content:        hprp.TextContent{Type: hprp.ContentTypeText, Text: "prompt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan hprp.Envelope, 1)
	go func() {
		response, requestErr := connection.request(context.Background(), request, hprp.TypeCommandResult)
		if requestErr != nil {
			done <- hprp.Envelope{}
			return
		}
		done <- response
	}()
	select {
	case sent := <-connection.sendQueue:
		if sent.ID != "command-1" {
			t.Fatalf("sent = %#v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not queued")
	}
	response, err := hprp.NewEnvelope(hprp.TypeCommandResult, "result-1", "command-1", false, hprp.CommandResult{Outcome: hprp.OutcomeOK})
	if err != nil {
		t.Fatal(err)
	}
	if !connection.deliver(response) {
		t.Fatal("response was not delivered")
	}
	select {
	case got := <-done:
		if got.ID != "result-1" {
			t.Fatalf("response = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not complete")
	}
}

func TestClientConnectionDeduplicatesAndOrdersCommandOutput(t *testing.T) {
	connection := newClientConnection(context.Background(), "connection-1", "cred-test", ClientKey{UserID: "user-a", MachineID: "home-mac"}, nil, 1, 2, slog.Default())
	connection.setCapabilities([]string{hprp.CapabilityCommandOutputV1})
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	if err := connection.registerCommand("command-1", target, time.Minute); err != nil {
		t.Fatal(err)
	}
	envelope := hprp.Envelope{ReplyTo: "command-1"}
	output := hprp.CommandOutput{Target: target, Sequence: 1, Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "part"}}
	if accepted, err := connection.acceptCommandOutput(envelope, output); err != nil || !accepted {
		t.Fatalf("first output = %v, %v", accepted, err)
	}
	if accepted, err := connection.acceptCommandOutput(envelope, output); err != nil || accepted {
		t.Fatalf("duplicate output = %v, %v", accepted, err)
	}
	output.Sequence = 3
	if accepted, err := connection.acceptCommandOutput(envelope, output); err == nil || accepted {
		t.Fatalf("out-of-order output = %v, %v", accepted, err)
	}
}

func TestClientConnectionRejectsUnnegotiatedCommandOutput(t *testing.T) {
	connection := newClientConnection(context.Background(), "connection-1", "cred-test", ClientKey{UserID: "user-a", MachineID: "home-mac"}, nil, 1, 2, slog.Default())
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	if err := connection.registerCommand("command-1", target, time.Minute); err != nil {
		t.Fatal(err)
	}
	accepted, err := connection.acceptCommandOutput(hprp.Envelope{ReplyTo: "command-1"}, hprp.CommandOutput{
		Target: target, Sequence: 1, Final: true,
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "part"},
	})
	if err == nil || accepted {
		t.Fatalf("acceptCommandOutput() = %v, %v, want capability error", accepted, err)
	}
}

func TestClientConnectionDeduplicatesAndOrdersNotificationsPerTarget(t *testing.T) {
	connection := newClientConnection(context.Background(), "connection-1", "cred-test", ClientKey{UserID: "user-a", MachineID: "home-mac"}, nil, 1, 2, slog.Default())
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	event := hprp.NotificationEvent{
		EventKey: "event-1", Sequence: 1, Kind: "agent.status", Target: target,
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "Agent 已完成"},
	}
	if !connection.acceptNotification(event) {
		t.Fatal("first notification was rejected")
	}
	if connection.acceptNotification(event) {
		t.Fatal("duplicate notification was accepted")
	}
	event.EventKey = "event-2"
	if connection.acceptNotification(event) {
		t.Fatal("duplicate notification sequence was accepted")
	}
	event.Sequence = 2
	if !connection.acceptNotification(event) {
		t.Fatal("next notification sequence was rejected")
	}
}
