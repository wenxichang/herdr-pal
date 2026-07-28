package hprp

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateClientHelloAllowsUnknownFeatureParameters(t *testing.T) {
	hello := validClientHello()
	hello.Features = map[string]FeatureOffer{
		"terminal.inspect.v1": {Parameters: map[string]json.RawMessage{
			"future_parameter": json.RawMessage(`{"enabled":true}`),
		}},
	}
	if err := ValidateClientHello(hello); err != nil {
		t.Fatalf("ValidateClientHello() error = %v", err)
	}
}

func TestValidateClientHelloRejectsInvalidExtensionName(t *testing.T) {
	hello := validClientHello()
	hello.Capabilities = []string{"command.output"}
	if err := ValidateClientHello(hello); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("ValidateClientHello() error = %v, want ErrInvalidMessage", err)
	}
}

func TestValidateTargetRequiresAllStableIdentities(t *testing.T) {
	valid := Target{MachineID: "office-pc", SlotID: "w1:p1", SessionID: "session-1"}
	if err := ValidateTarget(valid); err != nil {
		t.Fatalf("ValidateTarget() error = %v", err)
	}
	for _, invalid := range []Target{
		{SlotID: valid.SlotID, SessionID: valid.SessionID},
		{MachineID: valid.MachineID, SessionID: valid.SessionID},
		{MachineID: valid.MachineID, SlotID: valid.SlotID},
	} {
		if err := ValidateTarget(invalid); !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("ValidateTarget(%#v) error = %v, want ErrInvalidTarget", invalid, err)
		}
	}
}

func TestValidateSessionSnapshotAndNormalizeFutureStatus(t *testing.T) {
	snapshot := SessionSnapshot{Sequence: 1, Sessions: []Session{{
		SlotID: "w1:p1", SessionID: "session-1",
		Display: SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex", Workspace: "test", Tab: "main", Title: "main-codex"},
		Status:  "future-status",
	}}}
	if err := ValidateSessionSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateSessionSnapshot() error = %v", err)
	}
	if got := NormalizeStatus(snapshot.Sessions[0].Status); got != StatusUnknown {
		t.Fatalf("NormalizeStatus() = %q, want %q", got, StatusUnknown)
	}
}

func TestValidateSessionSnapshotRejectsDuplicateSlot(t *testing.T) {
	session := Session{SlotID: "w1:p1", SessionID: "session-1", Display: SessionDisplay{Index: 1}, Status: StatusIdle}
	snapshot := SessionSnapshot{Sequence: 1, Sessions: []Session{session, session}}
	if err := ValidateSessionSnapshot(snapshot); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("ValidateSessionSnapshot() error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestValidateResultOutcomeUsesMessageSubset(t *testing.T) {
	if err := ValidateCommandResult(CommandResult{Outcome: OutcomeCancelled}); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("ValidateCommandResult() error = %v, want ErrInvalidOutcome", err)
	}
	if err := ValidateFeatureResult(FeatureResult{Feature: "workspace.prepare.v1", Outcome: OutcomeCancelled}); err != nil {
		t.Fatalf("ValidateFeatureResult() error = %v", err)
	}
	if err := ValidateFeatureCancelResult(FeatureCancelResult{Outcome: OutcomeIndeterminate}); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("ValidateFeatureCancelResult() error = %v, want ErrInvalidOutcome", err)
	}
}

func validClientHello() ClientHello {
	return ClientHello{
		Implementation: Implementation{Name: "herdr-pal", Version: "0.2.0", OS: "linux", Arch: "amd64"},
		Capabilities:   []string{"command.output.v1"},
		Features:       map[string]FeatureOffer{},
		Limits: ClientLimits{
			MaxReceiveMessageBytes: MaxMessageBytes,
			MaxInflightCommands:    8,
			MaxInflightFeatures:    4,
			IdempotencyWindowMS:    600000,
		},
	}
}
