package hprp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
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

func TestValidateServerHelloRejectsInvalidIdentityCapabilitiesAndLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ServerHello)
	}{
		{name: "empty connection", mutate: func(hello *ServerHello) { hello.ConnectionID = "" }},
		{name: "invalid machine", mutate: func(hello *ServerHello) { hello.MachineID = "bad machine" }},
		{name: "unversioned capability", mutate: func(hello *ServerHello) { hello.Capabilities = []string{"command.output"} }},
		{name: "zero message limit", mutate: func(hello *ServerHello) { hello.Limits.MaxMessageBytes = 0 }},
		{name: "zero heartbeat", mutate: func(hello *ServerHello) { hello.Heartbeat.IdleTimeoutMS = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hello := validServerHello()
			test.mutate(&hello)
			if err := ValidateServerHello(hello); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("ValidateServerHello() error = %v, want ErrInvalidMessage", err)
			}
		})
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

func TestValidateCommandExecuteRequiresStableTargetIdempotencyAndText(t *testing.T) {
	valid := CommandExecute{
		IdempotencyKey: "message-1",
		Target:         Target{MachineID: "office-pc", SlotID: "w1:p1", SessionID: "session-1"},
		Content:        TextContent{Type: ContentTypeText, Text: "继续处理"},
		OutputMode:     OutputModeText,
	}
	if err := ValidateCommandExecute(valid); err != nil {
		t.Fatalf("ValidateCommandExecute() error = %v", err)
	}
	invalidKey := valid
	invalidKey.IdempotencyKey = ""
	if err := ValidateCommandExecute(invalidKey); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid idempotency error = %v", err)
	}
	invalidTarget := valid
	invalidTarget.Target.SessionID = ""
	if err := ValidateCommandExecute(invalidTarget); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("invalid target error = %v", err)
	}
	invalidContent := valid
	invalidContent.Content.Type = "text/html"
	if err := ValidateCommandExecute(invalidContent); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid content error = %v", err)
	}
	invalidMode := valid
	invalidMode.OutputMode = "html"
	if err := ValidateCommandExecute(invalidMode); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid output mode error = %v", err)
	}
}

func TestValidateTerminalContentRequiresPairedTextAndValidPNG(t *testing.T) {
	content := validTerminalImageContent(t)
	content.Text = string([]byte{0xff})
	if err := ValidateContent(content); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid text error = %v", err)
	}

	content = validTerminalImageContent(t)
	content.Image.Data = base64.StdEncoding.EncodeToString([]byte("not png"))
	if err := ValidateContent(content); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid png error = %v", err)
	}
}

func TestValidateTerminalContentRejectsInvalidMetadataAndLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Content)
	}{
		{name: "invalid mode", mutate: func(content *Content) { content.Mode = "html" }},
		{name: "missing image", mutate: func(content *Content) { content.Image = nil }},
		{name: "wrong width", mutate: func(content *Content) { content.Image.Width++ }},
		{name: "invalid base64", mutate: func(content *Content) { content.Image.Data = "%%%" }},
		{name: "invalid media type", mutate: func(content *Content) { content.Image.MediaType = "image/jpeg" }},
		{name: "invalid color mode", mutate: func(content *Content) { content.Image.ColorMode = "rgba" }},
		{name: "invalid page", mutate: func(content *Content) { content.Page.Current = content.Page.Total + 1 }},
		{name: "zero captured at", mutate: func(content *Content) { zero := time.Time{}; content.CapturedAt = &zero }},
		{name: "oversized text", mutate: func(content *Content) { content.Text = string(bytes.Repeat([]byte{'x'}, MaxTerminalTextBytes+1)) }},
		{name: "oversized image", mutate: func(content *Content) {
			content.Image.Data = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, MaxTerminalImageBytes+1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validTerminalImageContent(t)
			test.mutate(&content)
			if err := ValidateContent(content); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("ValidateContent() error = %v", err)
			}
		})
	}
}

func TestValidateTerminalTextContentAllowsEmptyScreen(t *testing.T) {
	content := validTerminalTextContent()
	content.Text = ""
	if err := ValidateContent(content); err != nil {
		t.Fatalf("ValidateContent() error = %v", err)
	}
}

func TestValidateNotificationEventUsesStructuredStatusData(t *testing.T) {
	event := validStatusEvent()
	if err := ValidateNotificationEvent(event); err != nil {
		t.Fatalf("ValidateNotificationEvent() error = %v", err)
	}

	invalid := event
	invalid.SnapshotSequence = 0
	if err := ValidateNotificationEvent(invalid); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("snapshot sequence error = %v", err)
	}
	invalid = event
	invalid.Data = &StatusChangeData{PreviousStatus: StatusDone, Status: StatusDone}
	if err := ValidateNotificationEvent(invalid); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("same status error = %v", err)
	}
}

func TestValidateTargetInvalidatedNotificationHasNoStatusData(t *testing.T) {
	event := validStatusEvent()
	event.Kind = NotificationKindTargetInvalidated
	event.Data = nil
	if err := ValidateNotificationEvent(event); err != nil {
		t.Fatalf("ValidateNotificationEvent() error = %v", err)
	}
	event.Data = &StatusChangeData{PreviousStatus: StatusWorking, Status: StatusDone}
	if err := ValidateNotificationEvent(event); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("unexpected data error = %v", err)
	}
}

func TestValidateTerminalSnapshotGetRejectsInvalidPurposeModeAndLimit(t *testing.T) {
	valid := TerminalSnapshotGet{
		Target: validTarget(), Mode: OutputModeImage,
		Purpose: TerminalSnapshotPurposeNotification, MaxLines: 100,
	}
	if err := ValidateTerminalSnapshotGet(valid); err != nil {
		t.Fatalf("ValidateTerminalSnapshotGet() error = %v", err)
	}
	for _, mutate := range []func(*TerminalSnapshotGet){
		func(request *TerminalSnapshotGet) { request.Mode = "html" },
		func(request *TerminalSnapshotGet) { request.Purpose = "interactive" },
		func(request *TerminalSnapshotGet) { request.MaxLines = 0 },
		func(request *TerminalSnapshotGet) { request.MaxLines = 101 },
	} {
		invalid := valid
		mutate(&invalid)
		if err := ValidateTerminalSnapshotGet(invalid); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("ValidateTerminalSnapshotGet(%#v) error = %v", invalid, err)
		}
	}
}

func TestValidateTerminalSnapshotResultAllowsSameReadFallback(t *testing.T) {
	fallback := validTerminalTextContent()
	result := TerminalSnapshotResult{
		Outcome:         OutcomeFailed,
		Target:          validTarget(),
		FallbackContent: &fallback,
		Error:           &Error{Code: CodeTerminalImageFailed, Retryable: false},
	}
	if err := ValidateTerminalSnapshotResult(result); err != nil {
		t.Fatalf("ValidateTerminalSnapshotResult() error = %v", err)
	}

	result.Error = &Error{Code: CodeTerminalSnapshotFailed, Retryable: true}
	if err := ValidateTerminalSnapshotResult(result); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("fallback error combination = %v", err)
	}
	result = TerminalSnapshotResult{
		Outcome: OutcomeOK, Target: validTarget(), Content: &fallback,
		Error: &Error{Code: CodeTerminalSnapshotFailed},
	}
	if err := ValidateTerminalSnapshotResult(result); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("successful result error combination = %v", err)
	}
}

func TestValidateNotificationEventRequiresIdentityOrderingAndTarget(t *testing.T) {
	valid := NotificationEvent{
		EventKey: "event-1", Sequence: 1, Kind: "agent.status",
		Target:  Target{MachineID: "office-pc", SlotID: "w1:p1", SessionID: "session-1"},
		Content: TextContent{Type: ContentTypeText, Text: "Agent 已完成"},
	}
	if err := ValidateNotificationEvent(valid); err != nil {
		t.Fatalf("ValidateNotificationEvent() error = %v", err)
	}
	for _, mutate := range []func(*NotificationEvent){
		func(event *NotificationEvent) { event.EventKey = "" },
		func(event *NotificationEvent) { event.Sequence = 0 },
		func(event *NotificationEvent) { event.Kind = "" },
	} {
		invalid := valid
		mutate(&invalid)
		if err := ValidateNotificationEvent(invalid); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("ValidateNotificationEvent(%#v) error = %v", invalid, err)
		}
	}
	invalidTarget := valid
	invalidTarget.Target.MachineID = ""
	if err := ValidateNotificationEvent(invalidTarget); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("invalid target error = %v", err)
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

func validServerHello() ServerHello {
	return ServerHello{
		ConnectionID: "connection-1", MachineID: "home-mac",
		Capabilities: []string{CapabilityCommandOutputV1}, Features: map[string]FeatureOffer{},
		Limits: ServerLimits{
			MaxMessageBytes: MaxMessageBytes, MaxSessions: MaxSessions, MaxInflightCommands: 1,
			MaxInflightFeatures: 0, MaxOutputBytes: MaxContentBytes, IdempotencyWindowMS: 600_000,
		},
		Heartbeat: HeartbeatConfig{PingIntervalMS: 20_000, IdleTimeoutMS: 60_000},
	}
}

func validTarget() Target {
	return Target{MachineID: "office-pc", SlotID: "w1:p1", SessionID: "session-1"}
}

func validStatusEvent() NotificationEvent {
	now := time.Now().UTC()
	return NotificationEvent{
		EventKey: "event-1", Sequence: 1, Kind: NotificationKindAgentStatusChanged,
		Target: validTarget(), SnapshotSequence: 3, OccurredAt: now,
		Data: &StatusChangeData{PreviousStatus: StatusWorking, Status: StatusDone},
	}
}

func validTerminalTextContent() Content {
	now := time.Now().UTC()
	return Content{
		Type: ContentTypeTerminal, Text: "terminal output", Mode: OutputModeText,
		Page: &TerminalPage{Current: 1, Total: 2}, CapturedAt: &now,
	}
}

func validTerminalImageContent(t *testing.T) Content {
	t.Helper()
	imageData := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
	imageData.SetColorIndex(1, 0, 1)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return Content{
		Type: ContentTypeTerminal, Text: "terminal output", Mode: OutputModeImage,
		Image: &TerminalImage{
			MediaType: "image/png", Encoding: "base64", Data: base64.StdEncoding.EncodeToString(encoded.Bytes()),
			Width: 2, Height: 1, ColorMode: "indexed-256",
		},
		Page: &TerminalPage{Current: 1, Total: 2}, CapturedAt: &now,
	}
}
