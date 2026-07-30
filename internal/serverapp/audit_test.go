package serverapp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/config"
	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestNewBusinessAuditorBuildsConfiguredOutputs(t *testing.T) {
	tests := []struct {
		name       string
		auditType  string
		stderr     bool
		wantOTLP   int32
		wantStderr bool
	}{
		{name: "none", auditType: "none"},
		{name: "stderr only", auditType: "none", stderr: true, wantStderr: true},
		{name: "otlp only", auditType: "otlp", wantOTLP: 1},
		{name: "otlp and stderr", auditType: "otlp", stderr: true, wantOTLP: 1, wantStderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				encoded, _ := proto.Marshal(&collectorlogsv1.ExportLogsServiceResponse{})
				_, _ = writer.Write(encoded)
			}))
			defer collector.Close()
			var stderr bytes.Buffer
			configured := config.AuditConfig{Type: test.auditType, Stderr: test.stderr}
			if test.auditType == "otlp" {
				configured.Endpoint = collector.URL + "/v1/logs"
			}
			auditor, redactor, err := newBusinessAuditor(configured, &stderr, slog.New(slog.NewTextHandler(io.Discard, nil)), "bot-secret", "v-test")
			if err != nil {
				t.Fatalf("newBusinessAuditor() error = %v", err)
			}
			event, err := audit.PrepareEvent(audit.Event{
				EventName: audit.EventNameUserInput, PrincipalID: "user-a", Outcome: "accepted",
				Timestamp: time.Unix(1, 0), Body: redactor.Redact("prompt bot-secret"),
			}, time.Unix(2, 0), bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
			if err != nil {
				t.Fatal(err)
			}
			auditor.Emit(event)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := auditor.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}
			if requests.Load() != test.wantOTLP {
				t.Fatalf("OTLP requests = %d, want %d", requests.Load(), test.wantOTLP)
			}
			hasStderr := strings.Contains(stderr.String(), audit.EventNameUserInput)
			if hasStderr != test.wantStderr {
				t.Fatalf("stderr = %q, want output %v", stderr.String(), test.wantStderr)
			}
			if strings.Contains(stderr.String(), "bot-secret") || event.Body != "prompt "+audit.RedactedValue {
				t.Fatalf("credential leaked: event=%#v stderr=%q", event, stderr.String())
			}
		})
	}
}

func TestNewBusinessAuditorRedactsOTLPHeaderValues(t *testing.T) {
	configured := config.AuditConfig{
		Type: "otlp", Endpoint: "http://127.0.0.1:4318/v1/logs",
		Headers: map[string]string{"Authorization": "Bearer collector-secret"},
	}
	auditor, redactor, err := newBusinessAuditor(configured, io.Discard, nil, "", "dev")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := auditor.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if output := redactor.Redact("Authorization: Bearer collector-secret"); strings.Contains(output, "collector-secret") {
		t.Fatalf("Redact() = %q", output)
	}
}
