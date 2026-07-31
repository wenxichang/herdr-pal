package audit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTLPExporterSendsProtobufLogsWithHeaders(t *testing.T) {
	requestReceived := make(chan *collectorlogsv1.ExportLogsServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("request = %s content-type=%q", request.Method, request.Header.Get("Content-Type"))
		}
		if request.URL.Path != "/otlp/v1/logs" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer collector-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload collectorlogsv1.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &payload); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		requestReceived <- &payload
		encoded, _ := proto.Marshal(&collectorlogsv1.ExportLogsServiceResponse{})
		writer.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = writer.Write(encoded)
	}))
	defer server.Close()

	exporter, err := NewOTLPExporter(OTLPConfig{
		Endpoint: server.URL + "/otlp/v1/logs", Headers: map[string]string{"Authorization": "Bearer collector-token"},
		ServiceVersion: "v-test", HostName: "host-test", ProcessID: 42,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewOTLPExporter() error = %v", err)
	}
	event := Event{
		SchemaVersion: 1, EventID: "event-1", EventName: EventNameUserInput,
		Timestamp: time.Unix(100, 2), ObservedTimestamp: time.Unix(101, 3),
		PrincipalID: "user-1", BotIDHash: "bot-hash", Action: "prompt", Outcome: "accepted",
		Agent: "codex",
		Body:  "full input", ContentBytes: 10, Attributes: map[string]string{"limit.window": "second"},
	}
	if err := exporter.Export(context.Background(), []Event{event}); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	payload := <-requestReceived
	if len(payload.ResourceLogs) != 1 || len(payload.ResourceLogs[0].ScopeLogs) != 1 || len(payload.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	resourceAttributes := keyValues(payload.ResourceLogs[0].Resource.Attributes)
	if resourceAttributes["service.name"] != "herdr-pal-server" || resourceAttributes["service.version"] != "v-test" || resourceAttributes["host.name"] != "host-test" || resourceAttributes["process.pid"] != "42" {
		t.Fatalf("resource attributes = %#v", resourceAttributes)
	}
	scope := payload.ResourceLogs[0].ScopeLogs[0].Scope
	if scope.Name != "github.com/wenxichang/herdr-pal/audit" || scope.Version != "v-test" {
		t.Fatalf("scope = %#v", scope)
	}
	record := payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if record.EventName != event.EventName || record.Body.GetStringValue() != event.Body || record.TimeUnixNano != uint64(event.Timestamp.UnixNano()) || record.ObservedTimeUnixNano != uint64(event.ObservedTimestamp.UnixNano()) {
		t.Fatalf("record = %#v", record)
	}
	attributes := keyValues(record.Attributes)
	if attributes["herdr_pal.audit.event_id"] != "event-1" || attributes["herdr_pal.audit.principal_id"] != "user-1" || attributes["herdr_pal.audit.agent"] != "codex" || attributes["herdr_pal.audit.limit.window"] != "second" {
		t.Fatalf("attributes = %#v", attributes)
	}
}

func TestOTLPExporterRetriesRetryableStatus(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		encoded, _ := proto.Marshal(&collectorlogsv1.ExportLogsServiceResponse{})
		_, _ = writer.Write(encoded)
	}))
	defer server.Close()
	exporter, err := NewOTLPExporter(OTLPConfig{
		Endpoint: server.URL + "/v1/logs", RequestTimeout: 100 * time.Millisecond,
		RetryLifetime: time.Second, RetryBaseDelay: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), []Event{{EventName: EventNameUserInput}}); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestOTLPExporterDoesNotRetryPermanentOrPartialResponses(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		response  *collectorlogsv1.ExportLogsServiceResponse
		wantError bool
	}{
		{name: "permanent", status: http.StatusBadRequest, wantError: true},
		{name: "partial", status: http.StatusOK, response: &collectorlogsv1.ExportLogsServiceResponse{PartialSuccess: &collectorlogsv1.ExportLogsPartialSuccess{RejectedLogRecords: 1, ErrorMessage: "sensitive collector detail"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				writer.WriteHeader(test.status)
				if test.response != nil {
					encoded, _ := proto.Marshal(test.response)
					_, _ = writer.Write(encoded)
				}
			}))
			defer server.Close()
			exporter, err := NewOTLPExporter(OTLPConfig{Endpoint: server.URL + "/v1/logs", RetryBaseDelay: time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			err = exporter.Export(context.Background(), []Event{{EventName: EventNameUserInput}})
			if (err != nil) != test.wantError {
				t.Fatalf("Export() error = %v", err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d", attempts.Load())
			}
		})
	}
}

func TestOTLPExporterSupportsSkipVerifyForHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		encoded, _ := proto.Marshal(&collectorlogsv1.ExportLogsServiceResponse{})
		_, _ = writer.Write(encoded)
	}))
	defer server.Close()
	exporter, err := NewOTLPExporter(OTLPConfig{Endpoint: server.URL + "/v1/logs", SkipVerify: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), []Event{{EventName: EventNameUserInput}}); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
}

func keyValues(values []*commonv1.KeyValue) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		switch typed := value.Value.Value.(type) {
		case *commonv1.AnyValue_StringValue:
			result[value.Key] = typed.StringValue
		case *commonv1.AnyValue_IntValue:
			result[value.Key] = stringValue(typed.IntValue)
		}
	}
	return result
}

func stringValue(value int64) string {
	return fmt.Sprintf("%d", value)
}
