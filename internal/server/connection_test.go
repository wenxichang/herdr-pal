package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

func TestClientConnectionLogsWriterEncodingFailureReason(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	connection := newClientConnection(context.Background(), "connection-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, nil, 1, 1, logger)
	go connection.runWriter()
	connection.sendQueue <- relayproto.Frame{Protocol: relayproto.ProtocolVersion + 1, Type: relayproto.TypePing}

	select {
	case <-connection.writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop")
	}
	output := logs.String()
	for _, want := range []string{"Relay 发送队列帧编码失败", "error_type=protocol_mismatch", "reason="} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "user-a") {
		t.Fatalf("logs leaked userid: %s", output)
	}
}
