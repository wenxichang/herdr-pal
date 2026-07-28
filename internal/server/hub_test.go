package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestHubAcceptsHelloAndFirstFullSnapshot(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Display.Title == "title"
	})
}

func TestHubRejectsNewDuplicateConnectionWithoutReplacingOld(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	first := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer first.Close()
	second := dialRawHubClient(t, server)
	defer second.Close()
	second.WriteFrame(t, relayproto.TypeClientHello, "hello-2", relayproto.ClientHello{UserID: "user-a", MachineID: "home-mac", ClientVersion: "test"})
	errorFrame := second.ReadFrame(t)
	if errorFrame.Type != relayproto.TypeProtocolError {
		t.Fatalf("frame type = %q", errorFrame.Type)
	}
	payload, err := relayproto.DecodePayload[relayproto.ProtocolErrorPayload](errorFrame)
	if err != nil || payload.Code != relayproto.CodeDuplicateClient || !payload.Close {
		t.Fatalf("protocol error = %#v, %v", payload, err)
	}
	if !hub.Catalog().HasMachine("user-a", "home-mac") {
		t.Fatal("old connection was displaced")
	}
}

func TestHubRemovesMachineImmediatelyWhenSocketCloses(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	client.Close()
	eventuallyHub(t, func() bool { return !hub.Catalog().HasMachine("user-a", "home-mac") })
}

func TestHubCorrelatesSelectAndExecuteResponses(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	target := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}

	selectDone := make(chan error, 1)
	go func() { selectDone <- hub.Select(context.Background(), "user-a", target) }()
	selectRequest := client.ReadFrame(t)
	if selectRequest.Type != relayproto.TypeSelectRequest || selectRequest.RequestID == "" {
		t.Fatalf("select request = %#v", selectRequest)
	}
	client.WriteFrame(t, relayproto.TypeSelectResult, selectRequest.RequestID, relayproto.SelectResult{OK: true})
	if err := <-selectDone; err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	executeDone := make(chan struct {
		result RelayExecution
		err    error
	}, 1)
	go func() {
		result, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "prompt"})
		executeDone <- struct {
			result RelayExecution
			err    error
		}{result, err}
	}()
	executeRequest := client.ReadFrame(t)
	if executeRequest.Type != relayproto.TypeExecuteRequest || executeRequest.RequestID == "" {
		t.Fatalf("execute request = %#v", executeRequest)
	}
	replacement := target
	replacement.OccupantHash = "occ-2"
	client.WriteFrame(t, relayproto.TypeExecuteResponse, executeRequest.RequestID, relayproto.ExecuteResponse{Content: "done", SelectedTarget: &replacement})
	result := <-executeDone
	if result.err != nil || result.result.Content != "done" || result.result.SelectedTarget == nil || *result.result.SelectedTarget != replacement {
		t.Fatalf("Execute() = %#v, %v", result.result, result.err)
	}
}

func TestHubMapsClientTargetRejection(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	target := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	done := make(chan error, 1)
	go func() { done <- hub.Select(context.Background(), "user-a", target) }()
	request := client.ReadFrame(t)
	client.WriteFrame(t, relayproto.TypeSelectResult, request.RequestID, relayproto.SelectResult{Code: relayproto.CodeTargetChanged, Message: "changed"})
	if err := <-done; !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Select() error = %v", err)
	}
}

func TestHubRejectsExecuteReplacementOutsideOriginalPane(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool { return len(hub.Catalog().CreateNumberedSnapshot("user-a")) == 1 })
	target := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	done := make(chan error, 1)
	go func() {
		_, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "prompt"})
		done <- err
	}()
	request := client.ReadFrame(t)
	replacement := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 2, PaneID: "pane-2", OccupantHash: "occ-2"}
	client.WriteFrame(t, relayproto.TypeExecuteResponse, request.RequestID, relayproto.ExecuteResponse{Content: "done", SelectedTarget: &replacement})
	if err := <-done; !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestHubDropsConnectionThatMissesFirstSnapshotDeadline(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{FirstSnapshotTimeout: 20 * time.Millisecond})
	client := dialRawHubClient(t, server)
	defer client.Close()
	client.WriteFrame(t, relayproto.TypeClientHello, "hello", relayproto.ClientHello{UserID: "user-a", MachineID: "home-mac", ClientVersion: "test"})
	if frame := client.ReadFrame(t); frame.Type != relayproto.TypeServerHello {
		t.Fatalf("server hello = %#v", frame)
	}
	eventuallyHub(t, func() bool { return !hub.Catalog().HasMachine("user-a", "home-mac") })
}

func TestHubHeartbeatTimeoutRemovesUnresponsiveClient(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 30 * time.Millisecond})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool { return !hub.Catalog().HasMachine("user-a", "home-mac") })
}

func TestHubDispatchesPushAndNotificationThroughBoundedOutboundSink(t *testing.T) {
	hub, relayServer := startHubServer(t, HubConfig{})
	sink := &hubOutboundRecorder{events: make(chan string, 2)}
	if err := hub.SetOutboundSink(sink); err != nil {
		t.Fatal(err)
	}
	client := dialReadyHubClient(t, relayServer, "user-a", "home-mac")
	defer client.Close()
	client.WriteFrame(t, relayproto.TypeExecutePush, "request-1", relayproto.ExecutePush{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"},
		Content: "push",
	})
	client.WriteFrame(t, relayproto.TypeNotification, "", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"},
		Content: "notice",
	})
	for _, want := range []string{"push:user-a:home-mac:push", "notification:user-a:home-mac:notice"} {
		select {
		case got := <-sink.events:
			if got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing outbound event %q", want)
		}
	}
}

func TestHubVerboseLogsHandshakeAndSnapshotReasonsWithoutSessionContent(t *testing.T) {
	logs := &lockedLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, relayServer := startHubServerWithLogger(t, HubConfig{}, logger)

	invalid := dialRawHubClient(t, relayServer)
	invalid.WriteFrame(t, relayproto.TypeClientHello, "hello-invalid", relayproto.ClientHello{MachineID: "home-mac", ClientVersion: "test"})
	_ = invalid.ReadFrame(t)
	invalid.Close()
	eventuallyHub(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "Relay 客户端握手失败") && strings.Contains(output, "stage=client_hello") &&
			strings.Contains(output, "error_type=invalid_identity")
	})

	client := dialRawHubClient(t, relayServer)
	defer client.Close()
	client.WriteFrame(t, relayproto.TypeClientHello, "hello", relayproto.ClientHello{UserID: "user-a", MachineID: "home-mac", ClientVersion: "test-version"})
	if frame := client.ReadFrame(t); frame.Type != relayproto.TypeServerHello {
		t.Fatalf("server hello = %#v", frame)
	}
	client.WriteFrame(t, relayproto.TypeSessionSnapshot, "", relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "private-panel-title")},
	})
	eventuallyHub(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "Relay 客户端已就绪") && strings.Contains(output, "client_version=test-version") &&
			strings.Contains(output, "snapshot_sequence=1") && strings.Contains(output, "session_count=1")
	})
	client.WriteFrame(t, relayproto.TypeSessionSnapshot, "", relayproto.SessionSnapshot{
		Sequence: 2, Sessions: []relayproto.Session{
			relaySession(1, "pane-1", "occ-1", "private-panel-title"),
			relaySession(2, "pane-2", "occ-2", "another-private-title"),
		},
	})
	eventuallyHub(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "Relay 客户端快照已上报") && strings.Contains(output, "snapshot_sequence=2") &&
			strings.Contains(output, "previous_session_count=1") && strings.Contains(output, "session_count=2") &&
			strings.Contains(output, "session_count_changed=true")
	})
	client.WriteFrame(t, relayproto.TypeSessionSnapshot, "", relayproto.SessionSnapshot{
		Sequence: 3, Sessions: []relayproto.Session{
			relaySession(1, "pane-1", "occ-1", "updated-private-title"),
			relaySession(2, "pane-2", "occ-2", "another-private-title"),
		},
	})
	eventuallyHub(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "snapshot_sequence=3") && strings.Contains(output, "session_count_changed=false")
	})
	output := logs.String()
	for _, forbidden := range []string{"private-panel-title", "updated-private-title", "another-private-title", "user-a"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, output)
		}
	}
	if !hub.Catalog().HasSessions("user-a") {
		t.Fatal("snapshot was not applied")
	}
}

func TestHubVerboseLogsOutboundEventAndExplicitWeComFailure(t *testing.T) {
	logs := &lockedLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, relayServer := startHubServerWithLogger(t, HubConfig{}, logger)
	if err := hub.SetOutboundSink(hubFailingOutboundSink{err: &wecom.ProtocolError{ErrCode: 93000}}); err != nil {
		t.Fatal(err)
	}
	client := dialReadyHubClient(t, relayServer, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool { return hub.Catalog().HasSessions("user-a") })
	client.WriteFrame(t, relayproto.TypeNotification, "", relayproto.Notification{
		Target:  relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"},
		Content: "private-terminal-output",
	})
	eventuallyHub(t, func() bool {
		output := logs.String()
		return strings.Contains(output, "Relay 客户端上报已接收") && strings.Contains(output, "event_type=notification") &&
			strings.Contains(output, "Relay 出站消息发送失败") && strings.Contains(output, "error_type=wecom_protocol") &&
			strings.Contains(output, "error_code=93000") && strings.Contains(output, "machine_id=home-mac") &&
			strings.Contains(output, "pane_id=pane-1") && strings.Contains(output, "occupant_hash=")
	})
	if output := logs.String(); strings.Contains(output, "private-terminal-output") || strings.Contains(output, "user-a") {
		t.Fatalf("logs leaked content or userid: %s", output)
	}
}

func startHubServer(t *testing.T, config HubConfig) (*ClientHub, *httptest.Server) {
	t.Helper()
	return startHubServerWithLogger(t, config, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
}

func startHubServerWithLogger(t *testing.T, config HubConfig, logger *slog.Logger) (*ClientHub, *httptest.Server) {
	t.Helper()
	hub, err := NewClientHub(NewSessionCatalog(), config, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(hub)
	t.Cleanup(server.Close)
	return hub, server
}

type hubFailingOutboundSink struct {
	err error
}

func (sink hubFailingOutboundSink) SendPush(context.Context, string, relayproto.ExecutePush) error {
	return sink.err
}

func (sink hubFailingOutboundSink) SendNotification(context.Context, string, string, relayproto.Notification) error {
	return sink.err
}

type lockedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedLogBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedLogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type hubTestClient struct {
	connection *websocket.Conn
}

func dialRawHubClient(t *testing.T, server *httptest.Server) *hubTestClient {
	t.Helper()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https")
	connection, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	connection.SetReadLimit(relayproto.MaxFrameBytes)
	return &hubTestClient{connection: connection}
}

func dialReadyHubClient(t *testing.T, server *httptest.Server, userID, machineID string) *hubTestClient {
	t.Helper()
	client := dialRawHubClient(t, server)
	client.WriteFrame(t, relayproto.TypeClientHello, "hello", relayproto.ClientHello{UserID: userID, MachineID: machineID, ClientVersion: "test"})
	if frame := client.ReadFrame(t); frame.Type != relayproto.TypeServerHello {
		t.Fatalf("server hello = %#v", frame)
	}
	client.WriteFrame(t, relayproto.TypeSessionSnapshot, "", relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	return client
}

func (client *hubTestClient) WriteFrame(t *testing.T, frameType relayproto.Type, requestID string, payload any) {
	t.Helper()
	frame, err := relayproto.NewFrame(frameType, requestID, payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := relayproto.Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func (client *hubTestClient) ReadFrame(t *testing.T) relayproto.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	messageType, data, err := client.connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v", messageType)
	}
	frame, err := relayproto.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func (client *hubTestClient) Close() {
	_ = client.connection.Close(websocket.StatusNormalClosure, "test complete")
}

func eventuallyHub(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}

type hubOutboundRecorder struct {
	events chan string
}

func (sink *hubOutboundRecorder) SendPush(_ context.Context, userID string, push relayproto.ExecutePush) error {
	sink.events <- "push:" + userID + ":" + push.Target.MachineID + ":" + push.Content
	return nil
}

func (sink *hubOutboundRecorder) SendNotification(_ context.Context, userID, machineID string, notification relayproto.Notification) error {
	sink.events <- "notification:" + userID + ":" + machineID + ":" + notification.Content
	return nil
}
