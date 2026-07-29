package hprp

import (
	"encoding/json"
	"testing"
)

func TestMessageTypesUsePublishedNames(t *testing.T) {
	want := map[Type]string{
		TypeHelloClient:            "hello.client",
		TypeHelloServer:            "hello.server",
		TypeSessionSnapshot:        "session.snapshot",
		TypeSessionSnapshotResult:  "session.snapshot.result",
		TypeCommandExecute:         "command.execute",
		TypeCommandResult:          "command.result",
		TypeCommandOutput:          "command.output",
		TypeNotificationEvent:      "notification.event",
		TypeFeatureInvoke:          "feature.invoke",
		TypeFeatureResult:          "feature.result",
		TypeFeatureEvent:           "feature.event",
		TypeFeatureCancel:          "feature.cancel",
		TypeFeatureCancelResult:    "feature.cancel.result",
		TypeTerminalSnapshotGet:    "terminal.snapshot.get",
		TypeTerminalSnapshotResult: "terminal.snapshot.result",
		TypeProtocolError:          "protocol.error",
	}
	for messageType, name := range want {
		if string(messageType) != name {
			t.Fatalf("message type = %q, want %q", messageType, name)
		}
	}
}

func TestTerminalCapabilitiesUsePublishedNames(t *testing.T) {
	if CapabilityTerminalSnapshotV1 != "terminal.snapshot.v1" {
		t.Fatalf("snapshot capability = %q", CapabilityTerminalSnapshotV1)
	}
	if CapabilityTerminalImageV1 != "terminal.image.v1" {
		t.Fatalf("image capability = %q", CapabilityTerminalImageV1)
	}
}

func TestFeatureMessagesKeepUserLevelObjectsOpaque(t *testing.T) {
	invoke := FeatureInvoke{
		Feature: "workspace.prepare.v1", IdempotencyKey: "msg-1",
		Target: json.RawMessage(`{"machine_id":"office-pc"}`),
		Input:  json.RawMessage(`{"repository":"herdr-pal"}`),
	}
	envelope, err := NewEnvelope(TypeFeatureInvoke, "feature-1", "", true, invoke)
	if err != nil {
		t.Fatalf("NewEnvelope() error = %v", err)
	}
	decoded, err := DecodePayload[FeatureInvoke](envelope)
	if err != nil || decoded.Feature != invoke.Feature || string(decoded.Target) != string(invoke.Target) {
		t.Fatalf("DecodePayload() = %#v, %v", decoded, err)
	}
}

func TestCommandOutputCarriesStableSource(t *testing.T) {
	output := CommandOutput{
		Target:   Target{MachineID: "office-pc", SlotID: "w1:p1", SessionID: "session-1"},
		Sequence: 1, Final: true, Content: TextContent{Type: ContentTypeText, Text: "完成"},
	}
	if err := ValidateCommandOutput(output); err != nil {
		t.Fatalf("ValidateCommandOutput() error = %v", err)
	}
}
