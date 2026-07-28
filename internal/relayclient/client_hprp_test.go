package relayclient

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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
	if err != nil || result.Content != "handled: prompt" {
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
				MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes, IdempotencyWindowMS: 600_000,
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
				MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes, IdempotencyWindowMS: 600_000,
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
