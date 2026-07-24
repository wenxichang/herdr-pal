package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

func TestHubAcceptsHelloAndFirstFullSnapshot(t *testing.T) {
	hub, server := startHubServer(t, HubConfig{})
	client := dialReadyHubClient(t, server, "user-a", "home-mac")
	defer client.Close()
	eventuallyHub(t, func() bool {
		entries := hub.Catalog().CreateNumberedSnapshot("user-a")
		return len(entries) == 1 && entries[0].Session.Title == "title"
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
		content string
		err     error
	}, 1)
	go func() {
		content, err := hub.Execute(context.Background(), "user-a", target, im.IncomingText{MessageID: "message-1", UserID: "user-a", Content: "prompt"})
		executeDone <- struct {
			content string
			err     error
		}{content, err}
	}()
	executeRequest := client.ReadFrame(t)
	if executeRequest.Type != relayproto.TypeExecuteRequest || executeRequest.RequestID == "" {
		t.Fatalf("execute request = %#v", executeRequest)
	}
	client.WriteFrame(t, relayproto.TypeExecuteResponse, executeRequest.RequestID, relayproto.ExecuteResponse{Content: "done"})
	result := <-executeDone
	if result.err != nil || result.content != "done" {
		t.Fatalf("Execute() = %q, %v", result.content, result.err)
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

func startHubServer(t *testing.T, config HubConfig) (*ClientHub, *httptest.Server) {
	t.Helper()
	hub, err := NewClientHub(NewSessionCatalog(), config, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(hub)
	t.Cleanup(server.Close)
	return hub, server
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
