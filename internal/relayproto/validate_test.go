package relayproto

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateClientHelloAcceptsCompositeIdentity(t *testing.T) {
	err := ValidateClientHello(ClientHello{UserID: "wm_xxxxxxxxx", MachineID: "home-mac_1", ClientVersion: "v0.1.0"})
	if err != nil {
		t.Fatalf("ValidateClientHello() error = %v", err)
	}
}

func TestValidateClientHelloRejectsInvalidIdentity(t *testing.T) {
	tests := []ClientHello{
		{UserID: "", MachineID: "home-mac", ClientVersion: "v0.1.0"},
		{UserID: strings.Repeat("u", MaxUserIDBytes+1), MachineID: "home-mac", ClientVersion: "v0.1.0"},
		{UserID: "user", MachineID: "has space", ClientVersion: "v0.1.0"},
		{UserID: "user", MachineID: strings.Repeat("m", MaxMachineIDBytes+1), ClientVersion: "v0.1.0"},
	}
	for _, hello := range tests {
		if err := ValidateClientHello(hello); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("ValidateClientHello(%#v) error = %v", hello, err)
		}
	}
}

func TestValidateSnapshotRejectsSequenceLimitAndDuplicateEntries(t *testing.T) {
	valid := Session{
		LocalIndex: 1, PaneID: "pane-1", TerminalID: "terminal-1", OccupantHash: "occupant-1",
		Agent: "codex", DisplayAgent: "Codex", Title: "实现 Relay", Workspace: "herdr-pal", Tab: "main", Status: "working",
	}
	tests := []struct {
		name     string
		snapshot SessionSnapshot
		want     error
	}{
		{name: "zero sequence", snapshot: SessionSnapshot{Sessions: []Session{valid}}, want: ErrInvalidSnapshot},
		{name: "too many sessions", snapshot: SessionSnapshot{Sequence: 1, Sessions: make([]Session, MaxSessionsPerSnapshot+1)}, want: ErrLimitExceeded},
		{name: "duplicate local index", snapshot: SessionSnapshot{Sequence: 1, Sessions: []Session{valid, func() Session { duplicate := valid; duplicate.PaneID = "pane-2"; return duplicate }()}}, want: ErrInvalidSnapshot},
		{name: "duplicate pane", snapshot: SessionSnapshot{Sequence: 1, Sessions: []Session{valid, func() Session { duplicate := valid; duplicate.LocalIndex = 2; return duplicate }()}}, want: ErrInvalidSnapshot},
		{name: "invalid status", snapshot: SessionSnapshot{Sequence: 1, Sessions: []Session{func() Session { invalid := valid; invalid.Status = "future"; return invalid }()}}, want: ErrInvalidSnapshot},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSnapshot(test.snapshot); !errors.Is(err, test.want) {
				t.Fatalf("ValidateSnapshot() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateSnapshotAcceptsEmptyFullReplacement(t *testing.T) {
	if err := ValidateSnapshot(SessionSnapshot{Sequence: 3, Sessions: []Session{}}); err != nil {
		t.Fatalf("ValidateSnapshot() error = %v", err)
	}
}

func TestValidateSessionRefRequiresStableTarget(t *testing.T) {
	valid := SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occupant-1"}
	if err := ValidateSessionRef(valid); err != nil {
		t.Fatalf("ValidateSessionRef() error = %v", err)
	}
	valid.OccupantHash = ""
	if err := ValidateSessionRef(valid); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("ValidateSessionRef() error = %v", err)
	}
}
