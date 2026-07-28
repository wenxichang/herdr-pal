package adminserver

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestAuditIncludesIdentityMethodTargetOutcomeAndExcludesSensitivePayloads(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server, err := NewServer(ServerConfig{
		Logger: logger,
		Handler: HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
			response, resultErr := adminproto.NewResultResponse(request.ID, struct{}{})
			return HandleResult{Response: response, AuditTarget: "credential_id=7"}, resultErr
		}),
		ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, done := startAdminConnection(t, server)
	request := adminproto.Request{
		Protocol: adminproto.Protocol,
		ID:       "request-secret-id",
		Method:   adminproto.MethodKeyDisable,
		Params:   []byte(`{"token":"hpk_7_secret-material","secret_sha256":"digest-sensitive","prompt":"prompt-sensitive","terminal":"terminal-sensitive","bot_secret":"bot-sensitive"}`),
	}
	writeAdminRequest(t, client, request)
	readAdminResponse(t, bufio.NewReader(client))
	client.Close()
	awaitConnectionDone(t, done)
	output := logs.String()
	for _, want := range []string{"HPAP 管理请求", "peer_uid=1000", "method=key.disable", `target="credential_id=7"`, "outcome=success", "duration="} {
		if !strings.Contains(output, want) {
			t.Fatalf("audit logs = %q, want %q", output, want)
		}
	}
	for _, forbidden := range []string{"request-secret-id", "hpk_7_secret-material", "digest-sensitive", "prompt-sensitive", "terminal-sensitive", "bot-sensitive"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("audit logs leaked %q: %q", forbidden, output)
		}
	}
}

func TestAuditRecordsStableErrorCodeWithoutHandlerErrorText(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	server, err := NewServer(ServerConfig{
		Logger: logger,
		Handler: HandlerFunc(func(context.Context, adminproto.Request) (HandleResult, error) {
			return HandleResult{}, sensitiveHandlerError("hpk_9_do-not-log")
		}),
		ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, done := startAdminConnection(t, server)
	writeAdminRequest(t, client, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-error", Method: adminproto.MethodServerStatus})
	response := readAdminResponse(t, bufio.NewReader(client))
	if response.Error == nil || response.Error.Code != adminproto.CodeServerInternal {
		t.Fatalf("handler error response = %#v", response)
	}
	client.Close()
	awaitConnectionDone(t, done)
	output := logs.String()
	if !strings.Contains(output, "error_code=server.internal") || strings.Contains(output, "hpk_9_do-not-log") {
		t.Fatalf("audit logs = %q", output)
	}
}

type sensitiveHandlerError string

func (err sensitiveHandlerError) Error() string { return string(err) }
