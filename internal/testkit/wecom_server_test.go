package testkit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWeComServerCorrelatesReqIDRecordsPingAndReturnsConfiguredError(t *testing.T) {
	server := NewWeComServer(t, "bot-1", "secret-1")
	connection, _, err := websocket.Dial(context.Background(), server.Endpoint(), nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test completed")
	server.WaitConnectionCount(t, 1)

	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_subscribe", "headers": map[string]any{"req_id": "subscribe-1"},
		"body": map[string]any{"bot_id": "bot-1", "secret": "secret-1"},
	})
	assertWeComTestResponse(t, connection, "subscribe-1", 0)
	server.WaitSubscribeCount(t, 1)

	server.SetResponseError("ping", 45009)
	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "ping", "headers": map[string]any{"req_id": "ping-1"}, "body": map[string]any{},
	})
	assertWeComTestResponse(t, connection, "ping-1", 45009)
	requests := server.WaitRequestCount(t, "ping", 1)
	if requests[0].RequestID != "ping-1" {
		t.Fatalf("ping request = %#v", requests[0])
	}

	server.Disconnect()
	server.WaitConnectionCount(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := connection.Read(ctx); err == nil {
		t.Fatal("Disconnect() 后 Read() error = nil")
	}
}

func writeWeComTestFrame(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func assertWeComTestResponse(t *testing.T, connection *websocket.Conn, requestID string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	var response struct {
		Headers struct {
			RequestID string `json:"req_id"`
		} `json:"headers"`
		ErrCode int `json:"errcode"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if response.Headers.RequestID != requestID || response.ErrCode != code {
		t.Fatalf("response = %#v, want req_id=%q errcode=%d", response, requestID, code)
	}
}
