package relayclient

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestClientAdvertisesTerminalCapabilities(t *testing.T) {
	helloReceived := make(chan hprp.ClientHello, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		helloEnvelope := readClientHPRPEnvelope(t, connection)
		hello, err := hprp.DecodePayload[hprp.ClientHello](helloEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		completeClientHandshake(t, connection, helloEnvelope.ID, terminalCapabilities())
		helloReceived <- hello
		<-t.Context().Done()
	})
	client, executor, cancel, done := startScriptedClient(t, relayServer, &terminalExecutor{})
	_ = client
	_ = executor
	defer stopScriptedClient(cancel, done)

	hello := <-helloReceived
	for _, capability := range terminalCapabilities() {
		if !slices.Contains(hello.Capabilities, capability) {
			t.Fatalf("hello.capabilities = %#v, missing %q", hello.Capabilities, capability)
		}
	}
}

func TestClientMapsImageTerminalReplyToCommandResult(t *testing.T) {
	resultReceived := make(chan hprp.CommandResult, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		writeClientHPRPEnvelope(t, connection, hprp.TypeCommandExecute, "command-image", "", hprp.CommandExecute{
			IdempotencyKey: "message-image",
			Target:         hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"},
			Content:        hprp.Content{Type: hprp.ContentTypeText, Text: "/con"},
			OutputMode:     hprp.OutputModeImage,
		}, true)
		resultEnvelope := readClientHPRPEnvelope(t, connection)
		result, err := hprp.DecodePayload[hprp.CommandResult](resultEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		resultReceived <- result
	})
	executor := &terminalExecutor{commandContent: imageTerminalContent(t, "审计文本")}
	_, _, cancel, done := startScriptedClient(t, relayServer, executor)
	defer stopScriptedClient(cancel, done)

	result := <-resultReceived
	if result.Outcome != hprp.OutcomeOK || result.Content == nil || result.Content.Type != hprp.ContentTypeTerminal ||
		result.Content.Mode != hprp.OutputModeImage || result.Content.Text != "审计文本" || result.Content.Image == nil {
		t.Fatalf("command result = %#v", result)
	}
}

func TestClientStatusEventContainsConfirmedSnapshotSequenceAndNoContent(t *testing.T) {
	envelopeReceived := make(chan hprp.Envelope, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		envelopeReceived <- readClientHPRPEnvelope(t, connection)
	})
	client, _, cancel, done := startScriptedClient(t, relayServer, &terminalExecutor{})
	defer stopScriptedClient(cancel, done)
	event := im.NotificationEvent{
		Kind: im.NotificationKindAgentStatusChanged, PreviousStatus: hprp.StatusWorking,
		Status: hprp.StatusDone, OccurredAt: time.Now().UTC(),
	}
	eventuallyClient(t, func() bool {
		return client.SendNotification(context.Background(), im.NotificationTarget{
			PaneID: "pane-1", OccupantHash: "session-1", Agent: "codex",
		}, event) == nil
	})

	envelope := <-envelopeReceived
	notification, err := hprp.DecodePayload[hprp.NotificationEvent](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if notification.Kind != hprp.NotificationKindAgentStatusChanged || notification.SnapshotSequence != 1 ||
		notification.Data == nil || notification.Data.PreviousStatus != hprp.StatusWorking || notification.Data.Status != hprp.StatusDone {
		t.Fatalf("notification = %#v", notification)
	}
	if bytes.Contains(envelope.Payload, []byte(`"content"`)) {
		t.Fatalf("status event contains content: %s", envelope.Payload)
	}
}

func TestClientSendsTargetInvalidatedAfterTargetLeavesLocalSnapshot(t *testing.T) {
	envelopeReceived := make(chan hprp.Envelope, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		envelopeReceived <- readClientHPRPEnvelope(t, connection)
	})
	client, executor, cancel, done := startScriptedClient(t, relayServer, &terminalExecutor{})
	defer stopScriptedClient(cancel, done)
	eventuallyClient(t, func() bool { return client.currentSession() != nil })
	executor.SetTargets(nil)

	err := client.SendNotification(context.Background(), im.NotificationTarget{
		PaneID: "pane-1", OccupantHash: "session-1", Agent: "codex",
	}, im.NotificationEvent{Kind: im.NotificationKindTargetInvalidated, OccurredAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}

	envelope := <-envelopeReceived
	notification, err := hprp.DecodePayload[hprp.NotificationEvent](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if notification.Kind != hprp.NotificationKindTargetInvalidated || notification.Data != nil ||
		notification.Target.SlotID != "pane-1" || notification.Target.SessionID != "session-1" {
		t.Fatalf("notification = %#v", notification)
	}
}

func TestClientHandlesTerminalSnapshotGetWithoutChangingSelection(t *testing.T) {
	resultReceived := make(chan hprp.TerminalSnapshotResult, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		writeClientHPRPEnvelope(t, connection, hprp.TypeTerminalSnapshotGet, "snapshot-get", "", hprp.TerminalSnapshotGet{
			Target: hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"},
			Mode:   hprp.OutputModeImage, Purpose: hprp.TerminalSnapshotPurposeNotification, MaxLines: 100,
		}, true)
		resultEnvelope := readClientHPRPEnvelope(t, connection)
		result, err := hprp.DecodePayload[hprp.TerminalSnapshotResult](resultEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		resultReceived <- result
	})
	executor := &terminalExecutor{snapshotContent: imageTerminalContent(t, "快照文本")}
	executor.selected = fakeSelection{PaneID: "pane-1", OccupantHash: "session-1"}
	_, _, cancel, done := startScriptedClient(t, relayServer, executor)
	defer stopScriptedClient(cancel, done)

	result := <-resultReceived
	if result.Outcome != hprp.OutcomeOK || result.Content == nil || result.Content.Text != "快照文本" || result.Content.Image == nil {
		t.Fatalf("terminal snapshot result = %#v", result)
	}
	if selected := executor.Selected(); selected != (fakeSelection{PaneID: "pane-1", OccupantHash: "session-1"}) {
		t.Fatalf("terminal snapshot changed selection: %#v", selected)
	}
}

func TestValidateInboundRequestEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		envelope hprp.Envelope
		wantErr  bool
	}{
		{name: "valid", envelope: hprp.Envelope{MustUnderstand: true}},
		{name: "reply is not a request", envelope: hprp.Envelope{ReplyTo: "request-1", MustUnderstand: true}, wantErr: true},
		{name: "request is optional", envelope: hprp.Envelope{}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateInboundRequestEnvelope(test.envelope)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateInboundRequestEnvelope() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestClientTerminalSnapshotImageFailureReturnsSameReadFallback(t *testing.T) {
	resultReceived := make(chan hprp.TerminalSnapshotResult, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		writeClientHPRPEnvelope(t, connection, hprp.TypeTerminalSnapshotGet, "snapshot-fallback", "", hprp.TerminalSnapshotGet{
			Target: hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"},
			Mode:   hprp.OutputModeImage, Purpose: hprp.TerminalSnapshotPurposeNotification, MaxLines: 100,
		}, true)
		resultEnvelope := readClientHPRPEnvelope(t, connection)
		result, err := hprp.DecodePayload[hprp.TerminalSnapshotResult](resultEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		resultReceived <- result
	})
	executor := &terminalExecutor{
		snapshotContent: im.TerminalContent{Mode: im.OutputModeText, Text: "同次读取文本", Page: &im.TerminalPage{Current: 1, Total: 1}, CapturedAt: time.Now().UTC()},
		snapshotErr:     errors.New("render failed"),
	}
	_, _, cancel, done := startScriptedClient(t, relayServer, executor)
	defer stopScriptedClient(cancel, done)

	result := <-resultReceived
	if result.Outcome != hprp.OutcomeFailed || result.Error == nil || result.Error.Code != hprp.CodeTerminalImageFailed ||
		result.FallbackContent == nil || result.FallbackContent.Text != "同次读取文本" || result.FallbackContent.Mode != hprp.OutputModeText {
		t.Fatalf("terminal snapshot fallback = %#v", result)
	}
}

func TestClientTerminalSnapshotImageFailureReturnsBlankSameReadFallback(t *testing.T) {
	resultReceived := make(chan hprp.TerminalSnapshotResult, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		writeClientHPRPEnvelope(t, connection, hprp.TypeTerminalSnapshotGet, "snapshot-blank-fallback", "", hprp.TerminalSnapshotGet{
			Target: hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"},
			Mode:   hprp.OutputModeImage, Purpose: hprp.TerminalSnapshotPurposeNotification, MaxLines: 100,
		}, true)
		resultEnvelope := readClientHPRPEnvelope(t, connection)
		result, err := hprp.DecodePayload[hprp.TerminalSnapshotResult](resultEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		resultReceived <- result
	})
	executor := &terminalExecutor{
		snapshotContent: im.TerminalContent{
			Mode: im.OutputModeText, Text: "", Page: &im.TerminalPage{Current: 1, Total: 1}, CapturedAt: time.Now().UTC(),
		},
		snapshotErr: errors.New("render failed"),
	}
	_, _, cancel, done := startScriptedClient(t, relayServer, executor)
	defer stopScriptedClient(cancel, done)

	result := <-resultReceived
	if result.Outcome != hprp.OutcomeFailed || result.Error == nil || result.Error.Code != hprp.CodeTerminalImageFailed ||
		result.FallbackContent == nil || result.FallbackContent.Text != "" || result.FallbackContent.Mode != hprp.OutputModeText {
		t.Fatalf("blank terminal snapshot fallback = %#v", result)
	}
}

func TestClientLimitsCommandAndTerminalSnapshotToOneInflightRequest(t *testing.T) {
	busyReceived := make(chan hprp.TerminalSnapshotResult, 1)
	firstReceived := make(chan hprp.TerminalSnapshotResult, 1)
	relayServer := newScriptedRelayServer(t, func(connection *websocket.Conn) {
		hello := readClientHPRPEnvelope(t, connection)
		completeClientHandshake(t, connection, hello.ID, terminalCapabilities())
		target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
		request := hprp.TerminalSnapshotGet{Target: target, Mode: hprp.OutputModeText, Purpose: hprp.TerminalSnapshotPurposeNotification, MaxLines: 100}
		writeClientHPRPEnvelope(t, connection, hprp.TypeTerminalSnapshotGet, "snapshot-first", "", request, true)
		writeClientHPRPEnvelope(t, connection, hprp.TypeTerminalSnapshotGet, "snapshot-second", "", request, true)
		for range 2 {
			envelope := readClientHPRPEnvelope(t, connection)
			result, err := hprp.DecodePayload[hprp.TerminalSnapshotResult](envelope)
			if err != nil {
				t.Fatal(err)
			}
			if envelope.ReplyTo == "snapshot-second" {
				busyReceived <- result
			} else {
				firstReceived <- result
			}
		}
	})
	executor := &terminalExecutor{
		snapshotContent: im.TerminalContent{Mode: im.OutputModeText, Text: "first", CapturedAt: time.Now().UTC()},
		snapshotStarted: make(chan struct{}), snapshotRelease: make(chan struct{}),
	}
	_, _, cancel, done := startScriptedClient(t, relayServer, executor)
	defer stopScriptedClient(cancel, done)
	<-executor.snapshotStarted
	busy := <-busyReceived
	if busy.Outcome != hprp.OutcomeFailed || busy.Error == nil || busy.Error.Code != hprp.CodeServerBusy {
		t.Fatalf("busy result = %#v", busy)
	}
	close(executor.snapshotRelease)
	first := <-firstReceived
	if first.Outcome != hprp.OutcomeOK || first.Content == nil || first.Content.Text != "first" {
		t.Fatalf("first result = %#v", first)
	}
}

func TestHPRPClientAuthenticatesReportsSnapshotAndExecutesCommand(t *testing.T) {
	token, record, err := credential.Issue(1, "user-a", "home-mac", []credential.SourceRule{"127.0.0.1", "::1"}, nil, time.Now(), bytes.NewReader(make([]byte, 48)))
	if err != nil {
		t.Fatal(err)
	}
	verifier := hprpClientVerifier{record: record}
	hub, err := server.NewClientHub(server.NewSessionCatalog(), verifier, server.HubConfig{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	defer relayServer.Close()
	executor := &fakeExecutor{targets: []session.Target{{
		PaneID: "pane-1", OccupantKey: "session-1", Agent: "codex", Status: herdr.AgentStatusIdle,
	}}}
	client, err := New(Config{
		URL: "wss" + strings.TrimPrefix(relayServer.URL, "https"), Key: token, SkipVerify: true,
		Version: "test", PollInterval: 10 * time.Millisecond, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyClient(t, func() bool { return hub.Catalog().HasSessions("user-a") })
	entries := hub.Catalog().CreateNumberedSnapshot("user-a")
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	result, err := hub.Execute(context.Background(), "user-a", entries[0].Ref, imMessage("message-hprp", "prompt"))
	if err != nil || result.StructuredContent == nil || result.StructuredContent.Text != "handled: prompt" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
}

func TestHPRPClientReportsUnknownRequiredMessageBeforeDisconnect(t *testing.T) {
	received := make(chan hprp.Envelope, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{hprp.Subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "test complete")
		hello := readClientHPRPEnvelope(t, connection)
		writeClientHPRPEnvelope(t, connection, hprp.TypeHelloServer, "hello-server", hello.ID, hprp.ServerHello{
			ConnectionID: "connection-1", MachineID: "home-mac", Capabilities: []string{}, Features: map[string]hprp.FeatureOffer{},
			Limits: hprp.ServerLimits{
				MaxMessageBytes: hprp.MaxMessageBytes, MaxSessions: hprp.MaxSessions, MaxInflightCommands: 1,
				MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes,
				MaxTerminalTextBytes: hprp.MaxTerminalTextBytes, MaxTerminalImageBytes: hprp.MaxTerminalImageBytes,
				IdempotencyWindowMS: 600_000,
			},
			Heartbeat: hprp.HeartbeatConfig{PingIntervalMS: 20_000, IdleTimeoutMS: 60_000},
		})
		snapshot := readClientHPRPEnvelope(t, connection)
		writeClientHPRPEnvelope(t, connection, hprp.TypeSessionSnapshotResult, "snapshot-result", snapshot.ID, hprp.SnapshotResult{
			Outcome: hprp.OutcomeOK, AppliedSequence: 1,
		})
		writeClientHPRPEnvelope(t, connection, hprp.Type("future.required"), "future-1", "", struct{}{}, true)
		received <- readClientHPRPEnvelope(t, connection)
	})
	relayServer := httptest.NewTLSServer(handler)
	defer relayServer.Close()
	client, err := New(Config{
		URL: "wss" + strings.TrimPrefix(relayServer.URL, "https"), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: time.Hour, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetExecutor(&fakeExecutor{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case envelope := <-received:
		cancel()
		if envelope.Type != hprp.TypeProtocolError || envelope.ReplyTo != "future-1" {
			t.Fatalf("protocol error = %#v", envelope)
		}
		payload, decodeErr := hprp.DecodePayload[hprp.ProtocolError](envelope)
		if decodeErr != nil || payload.Error.Code != hprp.CodeProtocolRequiredExtensionUnsupported || !payload.Close {
			t.Fatalf("protocol error payload = %#v, %v", payload, decodeErr)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("client did not report unknown required message")
	}
	<-done
}

func TestHPRPClientRejectsInvalidServerHelloBeforeSnapshot(t *testing.T) {
	var attempts atomic.Int32
	var snapshots atomic.Int32
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{hprp.Subprotocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		hello, err := readClientHPRPEnvelopeResult(request.Context(), connection)
		if err != nil {
			return
		}
		attempts.Add(1)
		if err := writeClientHPRPEnvelopeResult(request.Context(), connection, hprp.TypeHelloServer, "hello-invalid", hello.ID, hprp.ServerHello{
			ConnectionID: "connection-1", MachineID: "bad machine", Capabilities: []string{}, Features: map[string]hprp.FeatureOffer{},
			Limits: hprp.ServerLimits{
				MaxMessageBytes: hprp.MaxMessageBytes, MaxSessions: hprp.MaxSessions, MaxInflightCommands: 1,
				MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes,
				MaxTerminalTextBytes: hprp.MaxTerminalTextBytes, MaxTerminalImageBytes: hprp.MaxTerminalImageBytes,
				IdempotencyWindowMS: 600_000,
			},
			Heartbeat: hprp.HeartbeatConfig{PingIntervalMS: 20_000, IdleTimeoutMS: 60_000},
		}); err != nil {
			return
		}
		readContext, cancel := context.WithTimeout(request.Context(), 100*time.Millisecond)
		defer cancel()
		messageType, data, readErr := connection.Read(readContext)
		if readErr == nil && messageType == websocket.MessageText {
			envelope, decodeErr := hprp.Decode(data)
			if decodeErr == nil && envelope.Type == hprp.TypeSessionSnapshot {
				snapshots.Add(1)
			}
		}
	})
	relayServer := httptest.NewTLSServer(handler)
	defer relayServer.Close()
	token := relayClientTestKey(t)
	logs := &relayLockedLogBuffer{}
	client, err := New(Config{
		URL: "wss" + strings.TrimPrefix(relayServer.URL, "https") + "?access_token=private-query", Key: token, SkipVerify: true,
		Version: "test", PollInterval: time.Hour, SnapshotInterval: time.Hour,
		BackoffMin: time.Millisecond, BackoffMax: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetExecutor(&fakeExecutor{}); err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = client.Run(runContext)
	if attempts.Load() == 0 {
		t.Fatal("client did not reach hello.server")
	}
	if snapshots.Load() != 0 {
		t.Fatalf("client sent %d snapshots after invalid hello.server", snapshots.Load())
	}
	output := logs.String()
	for _, want := range []string{"stage=hello.server_decode", "error_type=protocol"} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	for _, forbidden := range []string{token, "user-a", "private-query"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, output)
		}
	}
}

func newScriptedRelayServer(t *testing.T, script func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	var once sync.Once
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{hprp.Subprotocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		once.Do(func() { script(connection) })
	})
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server
}

func terminalCapabilities() []string {
	return []string{
		hprp.CapabilityCommandOutputV1,
		hprp.CapabilityTerminalSnapshotV1,
		hprp.CapabilityTerminalImageV1,
	}
}

func completeClientHandshake(t *testing.T, connection *websocket.Conn, helloID string, capabilities []string) {
	t.Helper()
	writeClientHPRPEnvelope(t, connection, hprp.TypeHelloServer, "hello-server", helloID, hprp.ServerHello{
		ConnectionID: "connection-1", MachineID: "home-mac", Capabilities: capabilities, Features: map[string]hprp.FeatureOffer{},
		Limits: hprp.ServerLimits{
			MaxMessageBytes: hprp.MaxMessageBytes, MaxSessions: hprp.MaxSessions, MaxInflightCommands: 1,
			MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes,
			MaxTerminalTextBytes: hprp.MaxTerminalTextBytes, MaxTerminalImageBytes: hprp.MaxTerminalImageBytes,
			IdempotencyWindowMS: 600_000,
		},
		Heartbeat: hprp.HeartbeatConfig{PingIntervalMS: 20_000, IdleTimeoutMS: 60_000},
	})
	snapshot := readClientHPRPEnvelope(t, connection)
	writeClientHPRPEnvelope(t, connection, hprp.TypeSessionSnapshotResult, "snapshot-result", snapshot.ID, hprp.SnapshotResult{
		Outcome: hprp.OutcomeOK, AppliedSequence: 1,
	})
}

func startScriptedClient(
	t *testing.T,
	server *httptest.Server,
	executor *terminalExecutor,
) (*Client, *terminalExecutor, context.CancelFunc, <-chan error) {
	t.Helper()
	if len(executor.targets) == 0 {
		executor.targets = []session.Target{{
			PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "session-1", Agent: "codex",
			DisplayAgent: "Codex", Status: herdr.AgentStatusIdle,
		}}
	}
	client, err := New(Config{
		URL: "wss" + strings.TrimPrefix(server.URL, "https"), Key: relayClientTestKey(t), SkipVerify: true,
		Version: "test", PollInterval: time.Hour, SnapshotInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.sink = client
	if err := client.SetExecutor(executor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return client, executor, cancel, done
}

func stopScriptedClient(cancel context.CancelFunc, done <-chan error) {
	cancel()
	<-done
}

type terminalExecutor struct {
	mu              sync.Mutex
	targets         []session.Target
	selected        fakeSelection
	sink            any
	commandContent  im.TerminalContent
	snapshotContent im.TerminalContent
	snapshotErr     error
	snapshotStarted chan struct{}
	snapshotRelease chan struct{}
}

func (executor *terminalExecutor) CurrentTargets() []session.Target {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]session.Target(nil), executor.targets...)
}

func (executor *terminalExecutor) SetTargets(targets []session.Target) {
	executor.mu.Lock()
	executor.targets = append([]session.Target(nil), targets...)
	executor.mu.Unlock()
}

func (executor *terminalExecutor) SelectedTarget() (session.Target, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, target := range executor.targets {
		if target.PaneID == executor.selected.PaneID && target.OccupantKey == executor.selected.OccupantHash {
			return target, nil
		}
	}
	return session.Target{}, session.ErrNoSelection
}

func (executor *terminalExecutor) SelectTarget(paneID, occupantHash string) error {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, target := range executor.targets {
		if target.PaneID == paneID && target.OccupantKey == occupantHash {
			executor.selected = fakeSelection{PaneID: paneID, OccupantHash: occupantHash}
			return nil
		}
	}
	return session.ErrListSnapshotExpired
}

func (executor *terminalExecutor) HandleMessage(ctx context.Context, message im.IncomingText) {
	if sink, ok := executor.sink.(im.TerminalReplySink); ok {
		_ = sink.RespondTerminal(ctx, message.RequestID, executor.commandContent)
		return
	}
	_ = executor.sink.(im.ReplySink).RespondMarkdown(ctx, message.RequestID, "terminal sink unavailable")
}

func (executor *terminalExecutor) ReadTerminalSnapshot(
	ctx context.Context,
	_ string,
	_ string,
	_ im.OutputMode,
	_ int,
) (im.TerminalContent, error) {
	executor.mu.Lock()
	content, err := executor.snapshotContent, executor.snapshotErr
	started, release := executor.snapshotStarted, executor.snapshotRelease
	executor.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return im.TerminalContent{}, ctx.Err()
		case <-release:
		}
	}
	return content, err
}

func (executor *terminalExecutor) Selected() fakeSelection {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.selected
}

func imageTerminalContent(t *testing.T, text string) im.TerminalContent {
	t.Helper()
	paletted := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
	paletted.SetColorIndex(1, 0, 1)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, paletted); err != nil {
		t.Fatal(err)
	}
	return im.TerminalContent{
		Mode: im.OutputModeImage, Text: text,
		Image: &im.TerminalImage{
			MediaType: "image/png", Data: encoded.Bytes(), Width: 2, Height: 1, ColorMode: "indexed-256",
		},
		Page: &im.TerminalPage{Current: 1, Total: 1}, CapturedAt: time.Now().UTC(),
	}
}

func readClientHPRPEnvelopeResult(ctx context.Context, connection *websocket.Conn) (hprp.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return hprp.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return hprp.Envelope{}, hprp.ErrInvalidMessage
	}
	return hprp.Decode(data)
}

func writeClientHPRPEnvelopeResult(ctx context.Context, connection *websocket.Conn, messageType hprp.Type, id, replyTo string, payload any) error {
	envelope, err := hprp.NewEnvelope(messageType, id, replyTo, false, payload)
	if err != nil {
		return err
	}
	encoded, err := hprp.Encode(envelope)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, encoded)
}

func readClientHPRPEnvelope(t *testing.T, connection *websocket.Conn) hprp.Envelope {
	t.Helper()
	messageType, data, err := connection.Read(context.Background())
	if err != nil || messageType != websocket.MessageText {
		t.Fatalf("Read() = %v, %v", messageType, err)
	}
	envelope, err := hprp.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func writeClientHPRPEnvelope(t *testing.T, connection *websocket.Conn, messageType hprp.Type, id, replyTo string, payload any, mustUnderstand ...bool) {
	t.Helper()
	must := false
	if len(mustUnderstand) > 0 {
		must = mustUnderstand[0]
	}
	envelope, err := hprp.NewEnvelope(messageType, id, replyTo, must, payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := hprp.Encode(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

type hprpClientVerifier struct{ record credential.Record }

func (verifier hprpClientVerifier) VerifyBearer(_ context.Context, token string, source netip.Addr) (credential.Identity, error) {
	return credential.VerifyRecord(verifier.record, token, time.Now(), source)
}

func imMessage(messageID, content string) im.IncomingText {
	return im.IncomingText{MessageID: messageID, UserID: "user-a", ChatType: "single", Content: content}
}
