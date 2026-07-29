package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
)

func TestHPRPHubRequiresBearerAuthenticationBeforeUpgrade(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()

	_, response, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), Subprotocols: []string{hprp.Subprotocol},
	})
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Dial() response = %#v, error = %v, want HTTP 401", response, err)
	}
}

func TestHPRPHubLogsSafeAuthenticationContextWithoutTrustingForwardedHeaders(t *testing.T) {
	logs := &lockedHPRPLogBuffer{}
	hub, err := NewClientHub(NewSessionCatalog(), staticHPRPVerifier{err: credential.ErrUnauthenticated}, HubConfig{}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	header := http.Header{}
	token := "hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	header.Set("Authorization", "Bearer "+token)
	header.Set("X-Forwarded-For", "203.0.113.99")
	_, response, dialErr := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if dialErr == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Dial() response = %#v, error = %v", response, dialErr)
	}
	output := logs.String()
	for _, want := range []string{"HPRP 连接认证失败", "credential_id=12", "source_ip=127.0.0.1", "error_type=unauthenticated"} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	for _, forbidden := range []string{token, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "203.0.113.99"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, output)
		}
	}
}

func TestHPRPHubNegotiatesHelloAndAcknowledgesFirstSnapshot(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer test-key")
	connection, _, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	if connection.Subprotocol() != hprp.Subprotocol {
		t.Fatalf("subprotocol = %q", connection.Subprotocol())
	}

	writeHPRPTestEnvelope(t, connection, hprp.TypeHelloClient, "hello-1", "", hprp.ClientHello{
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "test", OS: "linux", Arch: "amd64"},
		Capabilities:   []string{hprp.CapabilityCommandOutputV1}, Features: map[string]hprp.FeatureOffer{},
		Limits: hprp.ClientLimits{MaxReceiveMessageBytes: hprp.MaxMessageBytes, MaxInflightCommands: 1, IdempotencyWindowMS: 60_000},
	})
	helloEnvelope := readHPRPTestEnvelope(t, connection)
	if helloEnvelope.Type != hprp.TypeHelloServer || helloEnvelope.ReplyTo != "hello-1" {
		t.Fatalf("hello envelope = %#v", helloEnvelope)
	}
	hello, err := hprp.DecodePayload[hprp.ServerHello](helloEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if hello.MachineID != "home-mac" || len(hello.Capabilities) != 1 || hello.Capabilities[0] != hprp.CapabilityCommandOutputV1 {
		t.Fatalf("hello payload = %#v", hello)
	}

	writeHPRPTestEnvelope(t, connection, hprp.TypeSessionSnapshot, "snapshot-1", "", hprp.SessionSnapshot{
		Sequence: 1,
		Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "session-1", Status: hprp.StatusIdle,
			Display: hprp.SessionDisplay{Index: 1, Agent: "codex", Title: "test"},
		}},
	})
	resultEnvelope := readHPRPTestEnvelope(t, connection)
	if resultEnvelope.Type != hprp.TypeSessionSnapshotResult || resultEnvelope.ReplyTo != "snapshot-1" {
		t.Fatalf("snapshot result envelope = %#v", resultEnvelope)
	}
	result, err := hprp.DecodePayload[hprp.SnapshotResult](resultEnvelope)
	if err != nil || result.Outcome != hprp.OutcomeOK || result.AppliedSequence != 1 {
		t.Fatalf("snapshot result = %#v, %v", result, err)
	}
	if !hub.Catalog().HasSessions("user-a") {
		t.Fatal("first snapshot was not published")
	}
}

func TestHPRPHubFetchTerminalSnapshotRoutesAndValidatesTarget(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	connection := connectReadyHPRPTestClient(t, server, []string{
		hprp.CapabilityCommandOutputV1, hprp.CapabilityTerminalSnapshotV1, hprp.CapabilityTerminalImageV1,
	})
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}
	if !hub.SupportsCapability("user-a", target, hprp.CapabilityTerminalImageV1) {
		t.Fatal("terminal.image.v1 was not negotiated")
	}

	type fetchResult struct {
		result hprp.TerminalSnapshotResult
		err    error
	}
	fetched := make(chan fetchResult, 1)
	go func() {
		result, err := hub.FetchTerminalSnapshot(context.Background(), "user-a", target, hprp.OutputModeText, 100)
		fetched <- fetchResult{result: result, err: err}
	}()
	requestEnvelope := readHPRPTestEnvelope(t, connection)
	if requestEnvelope.Type != hprp.TypeTerminalSnapshotGet {
		t.Fatalf("request envelope = %#v", requestEnvelope)
	}
	request, err := hprp.DecodePayload[hprp.TerminalSnapshotGet](requestEnvelope)
	if err != nil || request.Target != target || request.Mode != hprp.OutputModeText || request.MaxLines != 100 {
		t.Fatalf("terminal snapshot request = %#v, %v", request, err)
	}
	capturedAt := time.Now().UTC()
	writeHPRPTestEnvelope(t, connection, hprp.TypeTerminalSnapshotResult, "terminal-result", requestEnvelope.ID, hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeOK, Target: target,
		Content: &hprp.Content{
			Type: hprp.ContentTypeTerminal, Text: "terminal", Mode: hprp.OutputModeText,
			Page: &hprp.TerminalPage{Current: 1, Total: 1}, CapturedAt: &capturedAt,
		},
	})
	got := <-fetched
	if got.err != nil || got.result.Content == nil || got.result.Content.Text != "terminal" {
		t.Fatalf("FetchTerminalSnapshot() = %#v, %v", got.result, got.err)
	}
}

func TestHPRPHubRejectsTerminalSnapshotModeMismatch(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	connection := connectReadyHPRPTestClient(t, server, []string{
		hprp.CapabilityCommandOutputV1, hprp.CapabilityTerminalSnapshotV1, hprp.CapabilityTerminalImageV1,
	})
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}

	fetched := make(chan error, 1)
	go func() {
		_, err := hub.FetchTerminalSnapshot(context.Background(), "user-a", target, hprp.OutputModeImage, 100)
		fetched <- err
	}()
	requestEnvelope := readHPRPTestEnvelope(t, connection)
	capturedAt := time.Now().UTC()
	writeHPRPTestEnvelope(t, connection, hprp.TypeTerminalSnapshotResult, "terminal-result-mismatch", requestEnvelope.ID, hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeOK, Target: target,
		Content: &hprp.Content{
			Type: hprp.ContentTypeTerminal, Text: "terminal", Mode: hprp.OutputModeText,
			Page: &hprp.TerminalPage{Current: 1, Total: 1}, CapturedAt: &capturedAt,
		},
	})
	if err := <-fetched; err == nil {
		t.Fatal("FetchTerminalSnapshot() accepted mismatched output mode")
	}
}

func TestHPRPHubRejectsImageSnapshotWithoutCapability(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	connection := connectReadyHPRPTestClient(t, server, []string{hprp.CapabilityTerminalSnapshotV1})
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}

	if _, err := hub.FetchTerminalSnapshot(context.Background(), "user-a", target, hprp.OutputModeImage, 100); !errors.Is(err, ErrInvalidHubRequest) {
		t.Fatalf("FetchTerminalSnapshot(image) error = %v", err)
	}
}

func TestHPRPHubIgnoresLateCommandResultWithoutDisconnectingClient(t *testing.T) {
	hub := newHPRPTestHub(t)
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	connection := connectReadyHPRPTestClient(t, server, []string{
		hprp.CapabilityCommandOutputV1, hprp.CapabilityTerminalSnapshotV1,
	})
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	target := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "session-1"}

	ctx, cancel := context.WithCancel(context.Background())
	executeDone := make(chan error, 1)
	go func() {
		_, err := hub.Execute(ctx, "user-a", target, im.IncomingText{MessageID: "late-command", Content: "prompt"})
		executeDone <- err
	}()
	commandEnvelope := readHPRPTestEnvelope(t, connection)
	cancel()
	if err := <-executeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v", err)
	}
	capturedAt := time.Now().UTC()
	writeHPRPTestEnvelope(t, connection, hprp.TypeCommandResult, "late-command-result", commandEnvelope.ID, hprp.CommandResult{
		Outcome: hprp.OutcomeOK,
		Content: &hprp.Content{
			Type: hprp.ContentTypeTerminal, Mode: hprp.OutputModeText, Text: "late",
			CapturedAt: &capturedAt,
		},
	})

	fetchDone := make(chan error, 1)
	go func() {
		_, err := hub.FetchTerminalSnapshot(context.Background(), "user-a", target, hprp.OutputModeText, 100)
		fetchDone <- err
	}()
	snapshotEnvelope := readHPRPTestEnvelope(t, connection)
	if snapshotEnvelope.Type != hprp.TypeTerminalSnapshotGet {
		t.Fatalf("next envelope = %#v", snapshotEnvelope)
	}
	writeHPRPTestEnvelope(t, connection, hprp.TypeTerminalSnapshotResult, "snapshot-after-late", snapshotEnvelope.ID, hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeFailed, Target: target,
		Error: &hprp.Error{Code: hprp.CodeTerminalSnapshotFailed, Retryable: true},
	})
	if err := <-fetchDone; err != nil {
		t.Fatalf("FetchTerminalSnapshot() error = %v", err)
	}
}

func newHPRPTestHub(t *testing.T) *ClientHub {
	t.Helper()
	hub, err := NewClientHub(NewSessionCatalog(), staticHPRPVerifier{identity: credential.Identity{
		CredentialID: 1, PrincipalID: "user-a", MachineID: "home-mac",
	}}, HubConfig{}, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

func connectReadyHPRPTestClient(t *testing.T, server *httptest.Server, capabilities []string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer test-key")
	connection, _, err := websocket.Dial(context.Background(), hprpTestURL(server), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeHPRPTestEnvelope(t, connection, hprp.TypeHelloClient, "hello-ready", "", hprp.ClientHello{
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "test", OS: "linux", Arch: "amd64"},
		Capabilities:   capabilities, Features: map[string]hprp.FeatureOffer{},
		Limits: hprp.ClientLimits{MaxReceiveMessageBytes: hprp.MaxMessageBytes, MaxInflightCommands: 4, IdempotencyWindowMS: 60_000},
	})
	helloEnvelope := readHPRPTestEnvelope(t, connection)
	hello, err := hprp.DecodePayload[hprp.ServerHello](helloEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if !slices.Contains(hello.Capabilities, capability) {
			connection.CloseNow()
			t.Fatalf("hello.capabilities = %#v, missing %q", hello.Capabilities, capability)
		}
	}
	writeHPRPTestEnvelope(t, connection, hprp.TypeSessionSnapshot, "snapshot-ready", "", hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{{
			SlotID: "pane-1", SessionID: "session-1", Status: hprp.StatusIdle,
			Display: hprp.SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex", Title: "test"},
		}},
	})
	resultEnvelope := readHPRPTestEnvelope(t, connection)
	if resultEnvelope.Type != hprp.TypeSessionSnapshotResult {
		connection.CloseNow()
		t.Fatalf("snapshot result envelope = %#v", resultEnvelope)
	}
	return connection
}

type staticHPRPVerifier struct {
	identity credential.Identity
	err      error
}

func (verifier staticHPRPVerifier) VerifyBearer(_ context.Context, token string, _ netip.Addr) (credential.Identity, error) {
	if verifier.err != nil || token != "test-key" {
		return credential.Identity{}, credential.ErrUnauthenticated
	}
	return verifier.identity, nil
}

func hprpTestURL(server *httptest.Server) string {
	return "wss" + strings.TrimPrefix(server.URL, "https")
}

func writeHPRPTestEnvelope(t *testing.T, connection *websocket.Conn, messageType hprp.Type, id, replyTo string, payload any) {
	t.Helper()
	envelope, err := hprp.NewEnvelope(messageType, id, replyTo, false, payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := hprp.Encode(envelope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func readHPRPTestEnvelope(t *testing.T, connection *websocket.Conn) hprp.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v", messageType)
	}
	envelope, err := hprp.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
