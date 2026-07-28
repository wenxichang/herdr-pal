package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
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

func newHPRPTestHub(t *testing.T) *ClientHub {
	t.Helper()
	hub, err := NewClientHub(NewSessionCatalog(), staticHPRPVerifier{identity: credential.Identity{
		CredentialID: "cred-test", PrincipalID: "user-a", MachineID: "home-mac",
	}}, HubConfig{}, slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

type staticHPRPVerifier struct {
	identity credential.Identity
	err      error
}

func (verifier staticHPRPVerifier) VerifyBearer(_ context.Context, token string) (credential.Identity, error) {
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
