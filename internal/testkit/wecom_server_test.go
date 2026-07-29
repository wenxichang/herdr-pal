package testkit

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestWeComServerSupportsImageUploadLifecycle(t *testing.T) {
	server := NewWeComServer(t, "bot-1", "secret-1")
	connection, _, err := websocket.Dial(context.Background(), server.Endpoint(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test completed")
	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_subscribe", "headers": map[string]any{"req_id": "subscribe-1"},
		"body": map[string]any{"bot_id": "bot-1", "secret": "secret-1"},
	})
	assertWeComTestResponse(t, connection, "subscribe-1", 0)

	png := []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n', 1, 2, 3}
	digest := md5.Sum(png)
	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_upload_media_init", "headers": map[string]any{"req_id": "init-1"},
		"body": map[string]any{
			"type": "image", "filename": "herdr-terminal.png", "total_size": len(png),
			"total_chunks": 1, "md5": fmt.Sprintf("%x", digest),
		},
	})
	initBody := readWeComTestResponseBody(t, connection, "init-1")
	var initResult struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(initBody, &initResult); err != nil || initResult.UploadID == "" {
		t.Fatalf("init result = %#v, error = %v", initResult, err)
	}

	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_upload_media_chunk", "headers": map[string]any{"req_id": "chunk-1"},
		"body": map[string]any{"upload_id": initResult.UploadID, "chunk_index": 0, "base64_data": base64.StdEncoding.EncodeToString(png)},
	})
	assertWeComTestResponse(t, connection, "chunk-1", 0)
	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_upload_media_finish", "headers": map[string]any{"req_id": "finish-1"},
		"body": map[string]any{"upload_id": initResult.UploadID},
	})
	finishBody := readWeComTestResponseBody(t, connection, "finish-1")
	var finishResult struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(finishBody, &finishResult); err != nil || finishResult.MediaID == "" {
		t.Fatalf("finish result = %#v, error = %v", finishResult, err)
	}

	writeWeComTestFrame(t, connection, map[string]any{
		"cmd": "aibot_send_msg", "headers": map[string]any{"req_id": "send-1"},
		"body": map[string]any{
			"chatid": "user-1", "chat_type": 1, "msgtype": "image", "image": map[string]any{"media_id": finishResult.MediaID},
		},
	})
	assertWeComTestResponse(t, connection, "send-1", 0)

	chunks := server.WaitRequestCount(t, "aibot_upload_media_chunk", 1)
	if chunks[0].ChunkIndex != 0 || !bytes.Equal(chunks[0].Chunk, png) {
		t.Fatalf("chunk = %#v", chunks[0])
	}
	sends := server.WaitRequestCount(t, "aibot_send_msg", 1)
	if sends[0].MediaType != "image" || sends[0].MediaID != finishResult.MediaID || sends[0].ChatID != "user-1" {
		t.Fatalf("send = %#v", sends[0])
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

func readWeComTestResponseBody(t *testing.T, connection *websocket.Conn, requestID string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Headers struct {
			RequestID string `json:"req_id"`
		} `json:"headers"`
		ErrCode int             `json:"errcode"`
		Body    json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Headers.RequestID != requestID || response.ErrCode != 0 || len(response.Body) == 0 {
		t.Fatalf("response = %s", payload)
	}
	return response.Body
}
