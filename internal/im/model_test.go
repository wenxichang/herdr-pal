package im

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestNotificationTargetPreservesStableIdentity(t *testing.T) {
	target := NotificationTarget{
		PaneID:       "pane-1",
		OccupantHash: "occupant-1",
		Agent:        "codex",
		DisplayAgent: "Codex",
		Title:        "实现 Relay Server",
	}

	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, expected := range [][]byte{
		[]byte(`"pane_id":"pane-1"`),
		[]byte(`"occupant_hash":"occupant-1"`),
		[]byte(`"title":"实现 Relay Server"`),
	} {
		if !bytes.Contains(encoded, expected) {
			t.Fatalf("json.Marshal() = %s, want %s", encoded, expected)
		}
	}
}

func TestTerminalContentKeepsAuditTextWithImage(t *testing.T) {
	now := time.Now().UTC()
	content := TerminalContent{
		Mode: OutputModeImage, Text: "terminal text",
		Image: &TerminalImage{MediaType: "image/png", Data: []byte("png"), Width: 8, Height: 17, ColorMode: "indexed-256"},
		Page:  &TerminalPage{Current: 1, Total: 2}, CapturedAt: now,
	}
	if content.Text != "terminal text" || content.Image == nil || content.Page.Total != 2 || content.CapturedAt.IsZero() {
		t.Fatalf("TerminalContent = %#v", content)
	}
}

func TestTerminalReplySinkContract(t *testing.T) {
	var sink TerminalReplySink = terminalSinkStub{}
	if err := sink.RespondTerminal(context.Background(), "request-1", TerminalContent{Mode: OutputModeText}); err != nil {
		t.Fatal(err)
	}
}

type terminalSinkStub struct{}

func (terminalSinkStub) RespondTerminal(context.Context, string, TerminalContent) error { return nil }
func (terminalSinkStub) SendTerminal(context.Context, TerminalContent) error            { return nil }
