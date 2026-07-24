package im

import (
	"bytes"
	"encoding/json"
	"testing"
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
