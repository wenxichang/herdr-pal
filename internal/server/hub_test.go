package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestHPRPHubRejectsDuplicatePrincipalMachineBeforeUpgrade(t *testing.T) {
	_, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	first := dialHPRPRaw(t, server, "test-key")
	defer first.Close(websocket.StatusNormalClosure, "test complete")

	header := http.Header{}
	header.Set("Authorization", "Bearer test-key")
	_, response, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate Dial() response = %#v, error = %v, want HTTP 409", response, err)
	}
}

func TestHPRPHubShutdownRejectsNewConnectionsAndWaitsForActiveHandlers(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")

	hub.BeginShutdown()
	waitDone := make(chan error, 1)
	go func() { waitDone <- hub.Wait(context.Background()) }()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait() returned before active HPRP handler stopped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer test-key")
	connection, response, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("shutdown Dial() response = %#v, error = %v, want HTTP 503", response, err)
	}

	if disconnected := hub.DisconnectAll("server shutdown"); disconnected != 1 {
		t.Fatalf("DisconnectAll() = %d, want 1", disconnected)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() did not observe HPRP handler exit")
	}
}

func TestHPRPHubSelectIsLocalAndExecuteUsesStableTarget(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	if err := hub.Select(context.Background(), "user-a", target); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	type executeResult struct {
		result RelayExecution
		err    error
	}
	done := make(chan executeResult, 1)
	go func() {
		result, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "继续处理"})
		done <- executeResult{result: result, err: err}
	}()
	commandEnvelope := readHPRPTestEnvelope(t, client)
	if commandEnvelope.Type != hprp.TypeCommandExecute || !commandEnvelope.MustUnderstand {
		t.Fatalf("command envelope = %#v", commandEnvelope)
	}
	command, err := hprp.DecodePayload[hprp.CommandExecute](commandEnvelope)
	if err != nil || command.Target != target || command.IdempotencyKey != "message-1" || command.Content.Text != "继续处理" {
		t.Fatalf("command = %#v, %v", command, err)
	}
	writeHPRPTestEnvelope(t, client, hprp.TypeCommandResult, "result-1", commandEnvelope.ID, hprp.CommandResult{
		Outcome: hprp.OutcomeOK, Content: &hprp.TextContent{Type: hprp.ContentTypeText, Text: "已发送"},
	})
	select {
	case got := <-done:
		if got.err != nil || got.result.Content != "已发送" || got.result.SelectedTarget != nil {
			t.Fatalf("Execute() = %#v, %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not complete")
	}
}

func TestHPRPHubReturnsReplacementTarget(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-2"}
	done := make(chan RelayExecution, 1)
	go func() {
		result, executeErr := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-2", Content: "/slash clear"})
		if executeErr != nil {
			done <- RelayExecution{}
			return
		}
		done <- result
	}()
	command := readHPRPTestEnvelope(t, client)
	writeHPRPTestEnvelope(t, client, hprp.TypeSessionSnapshot, "snapshot-replacement", "", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "session-2", Status: hprp.StatusIdle,
			Display: hprp.SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex"},
		}},
	})
	if snapshotResult := readHPRPTestEnvelope(t, client); snapshotResult.Type != hprp.TypeSessionSnapshotResult {
		t.Fatalf("snapshot result = %#v", snapshotResult)
	}
	writeHPRPTestEnvelope(t, client, hprp.TypeCommandResult, "result-2", command.ID, hprp.CommandResult{
		Outcome: hprp.OutcomeOK, ReplacementTarget: &replacement,
	})
	select {
	case result := <-done:
		if result.SelectedTarget == nil || *result.SelectedTarget != replacement {
			t.Fatalf("replacement = %#v", result.SelectedTarget)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not complete")
	}
}

func TestHPRPHubRejectsReplacementTargetMissingFromConfirmedSnapshot(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-missing"}
	done := make(chan error, 1)
	go func() {
		_, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-missing-replacement", Content: "/slash clear"})
		done <- err
	}()
	command := readHPRPTestEnvelope(t, client)
	writeHPRPTestEnvelope(t, client, hprp.TypeCommandResult, "result-missing-replacement", command.ID, hprp.CommandResult{
		Outcome: hprp.OutcomeOK, ReplacementTarget: &replacement,
	})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing replacement target was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not reject missing replacement target")
	}
}

func TestHPRPHubRoutesCommandOutputAndNotification(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	sink := &hprpOutboundRecorder{outputs: make(chan hprp.CommandOutput, 1), notifications: make(chan hprp.NotificationEvent, 1)}
	if err := hub.SetOutboundSink(sink); err != nil {
		t.Fatal(err)
	}
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	done := make(chan error, 1)
	go func() {
		_, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-output", Content: "prompt"})
		done <- err
	}()
	command := readHPRPTestEnvelope(t, client)
	writeHPRPTestEnvelope(t, client, hprp.TypeCommandResult, "result-output", command.ID, hprp.CommandResult{Outcome: hprp.OutcomeOK})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	writeHPRPTestEnvelope(t, client, hprp.TypeCommandOutput, "output-1", command.ID, hprp.CommandOutput{
		Target: target, Sequence: 1, Final: true, Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "后续分段"},
	})
	writeHPRPTestEnvelope(t, client, hprp.TypeNotificationEvent, "notification-1", "", hprp.NotificationEvent{
		EventKey: "event-1", Sequence: 1, Kind: "agent.status", Target: target,
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "Agent 已完成"},
	})
	select {
	case output := <-sink.outputs:
		if output.Content.Text != "后续分段" || output.Target != target {
			t.Fatalf("output = %#v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("missing command output")
	}
	select {
	case notification := <-sink.notifications:
		if notification.Content.Text != "Agent 已完成" || notification.Target != target {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("missing notification")
	}
}

func TestHPRPHubRejectsNotificationForAnotherMachine(t *testing.T) {
	hub, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	sink := &hprpOutboundRecorder{outputs: make(chan hprp.CommandOutput, 1), notifications: make(chan hprp.NotificationEvent, 1)}
	if err := hub.SetOutboundSink(sink); err != nil {
		t.Fatal(err)
	}
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	writeHPRPTestEnvelope(t, client, hprp.TypeNotificationEvent, "notification-other-machine", "", hprp.NotificationEvent{
		EventKey: "event-other-machine", Sequence: 1, Kind: "agent.status",
		Target:  hprp.Target{MachineID: "office-pc", SlotID: "pane-1", SessionID: "session-1"},
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "不应转发"},
	})
	select {
	case notification := <-sink.notifications:
		t.Fatalf("cross-machine notification was forwarded: %#v", notification)
	case <-time.After(150 * time.Millisecond):
	}
	eventuallyHPRP(t, func() bool { return !hub.catalog.HasMachine("user-a", "home-mac") })
}

func TestHPRPHubAcknowledgesDuplicateAndRejectsStaleSnapshot(t *testing.T) {
	_, server := startHPRPHubServer(t, HubConfig{}, discardHPRPLogger())
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	snapshot := hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{{
		SlotID: "ignored", SessionID: "ignored", Status: hprp.StatusIdle, Display: hprp.SessionDisplay{Index: 1},
	}}}
	writeHPRPTestEnvelope(t, client, hprp.TypeSessionSnapshot, "snapshot-duplicate", "", snapshot)
	duplicate := readHPRPTestEnvelope(t, client)
	result, err := hprp.DecodePayload[hprp.SnapshotResult](duplicate)
	if err != nil || result.Outcome != hprp.OutcomeOK || result.AppliedSequence != 1 {
		t.Fatalf("duplicate result = %#v, %v", result, err)
	}

	writeHPRPTestEnvelope(t, client, hprp.TypeSessionSnapshot, "snapshot-2", "", hprp.SessionSnapshot{Sequence: 2, Sessions: snapshot.Sessions})
	_ = readHPRPTestEnvelope(t, client)
	writeHPRPTestEnvelope(t, client, hprp.TypeSessionSnapshot, "snapshot-stale", "", snapshot)
	stale := readHPRPTestEnvelope(t, client)
	result, err = hprp.DecodePayload[hprp.SnapshotResult](stale)
	if err != nil || result.Outcome != hprp.OutcomeRejected || result.Error == nil || result.Error.Code != hprp.CodeSyncStaleSnapshot {
		t.Fatalf("stale result = %#v, %v", result, err)
	}
}

func TestHPRPHubLogsOutboundFailureWithoutLeakingContentOrPrincipal(t *testing.T) {
	logs := &lockedHPRPLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, server := startHPRPHubServer(t, HubConfig{}, logger)
	if err := hub.SetOutboundSink(hprpFailingSink{err: &wecom.ProtocolError{ErrCode: 93000}}); err != nil {
		t.Fatal(err)
	}
	client := dialHPRPReady(t, server)
	defer client.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	writeHPRPTestEnvelope(t, client, hprp.TypeNotificationEvent, "notification-log", "", hprp.NotificationEvent{
		EventKey: "event-log", Sequence: 1, Kind: "agent.status", Target: target,
		Content: hprp.TextContent{Type: hprp.ContentTypeText, Text: "private-terminal-output"},
	})
	eventuallyHPRP(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "HPRP 出站消息发送失败") && strings.Contains(output, "error_type=wecom_protocol") &&
			strings.Contains(output, "error_code=93000") && strings.Contains(output, "machine_id=home-mac") && strings.Contains(output, "pane_id=pane-1")
	})
	if output := logs.String(); strings.Contains(output, "private-terminal-output") || strings.Contains(output, "user-a") || strings.Contains(output, "test-key") {
		t.Fatalf("logs leaked sensitive data: %s", output)
	}
}

func startHPRPHubServer(t *testing.T, config HubConfig, logger *slog.Logger) (*ClientHub, *httptest.Server) {
	t.Helper()
	hub, err := NewClientHub(NewSessionCatalog(), staticHPRPVerifier{identity: credential.Identity{
		CredentialID: 1, PrincipalID: "user-a", MachineID: "home-mac",
	}}, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(hub)
	t.Cleanup(server.Close)
	return hub, server
}

func dialHPRPRaw(t *testing.T, server *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	connection, _, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection.SetReadLimit(hprp.MaxMessageBytes)
	return connection
}

func dialHPRPReady(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	connection := dialHPRPRaw(t, server, "test-key")
	writeHPRPTestEnvelope(t, connection, hprp.TypeHelloClient, "hello-ready", "", hprp.ClientHello{
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "test", OS: "linux", Arch: "amd64"},
		Capabilities:   []string{hprp.CapabilityCommandOutputV1}, Features: map[string]hprp.FeatureOffer{},
		Limits: hprp.ClientLimits{MaxReceiveMessageBytes: hprp.MaxMessageBytes, MaxInflightCommands: 1, IdempotencyWindowMS: 60_000},
	})
	if hello := readHPRPTestEnvelope(t, connection); hello.Type != hprp.TypeHelloServer {
		t.Fatalf("hello = %#v", hello)
	}
	writeHPRPTestEnvelope(t, connection, hprp.TypeSessionSnapshot, "snapshot-ready", "", hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "session-1", Status: hprp.StatusIdle,
			Display: hprp.SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex", Workspace: "test", Tab: "main", Title: "title"},
		}},
	})
	if result := readHPRPTestEnvelope(t, connection); result.Type != hprp.TypeSessionSnapshotResult {
		t.Fatalf("snapshot result = %#v", result)
	}
	return connection
}

type hprpOutboundRecorder struct {
	outputs       chan hprp.CommandOutput
	notifications chan hprp.NotificationEvent
}

func (recorder *hprpOutboundRecorder) SendCommandOutput(_ context.Context, _ string, output hprp.CommandOutput) error {
	recorder.outputs <- output
	return nil
}

func (recorder *hprpOutboundRecorder) SendNotification(_ context.Context, _, _ string, notification hprp.NotificationEvent) error {
	recorder.notifications <- notification
	return nil
}

type hprpFailingSink struct{ err error }

func (sink hprpFailingSink) SendCommandOutput(context.Context, string, hprp.CommandOutput) error {
	return sink.err
}

func (sink hprpFailingSink) SendNotification(context.Context, string, string, hprp.NotificationEvent) error {
	return sink.err
}

type lockedHPRPLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedHPRPLogBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedHPRPLogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func eventuallyHPRP(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}

func discardHPRPLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testDiscardWriter{}, nil))
}
