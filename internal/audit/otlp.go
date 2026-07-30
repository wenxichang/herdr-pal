package audit

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	defaultOTLPRequestTimeout = 5 * time.Second
	defaultOTLPRetryLifetime  = 30 * time.Second
	defaultOTLPRetryBaseDelay = 250 * time.Millisecond
	maxOTLPResponseBytes      = 1 << 20
)

var ErrOTLPExport = errors.New("OTLP 审计输出失败")

// OTLPConfig 定义 OTLP/HTTP protobuf Logs 输出参数。
type OTLPConfig struct {
	Endpoint       string
	Headers        map[string]string
	SkipVerify     bool
	ServiceVersion string
	HostName       string
	ProcessID      int
	RequestTimeout time.Duration
	RetryLifetime  time.Duration
	RetryBaseDelay time.Duration
}

// OTLPExporter 使用官方 protobuf 类型发送 OTLP Logs 批次。
type OTLPExporter struct {
	config OTLPConfig
	client *http.Client
	logger *slog.Logger
}

// NewOTLPExporter 创建 OTLP/HTTP protobuf 输出器。
func NewOTLPExporter(config OTLPConfig, logger *slog.Logger) (*OTLPExporter, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 无效", ErrOTLPExport)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint 包含不允许的组件", ErrOTLPExport)
	}
	if config.SkipVerify && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: skip_verify 只允许用于 HTTPS", ErrOTLPExport)
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultOTLPRequestTimeout
	}
	if config.RetryLifetime <= 0 {
		config.RetryLifetime = defaultOTLPRetryLifetime
	}
	if config.RetryBaseDelay <= 0 {
		config.RetryBaseDelay = defaultOTLPRetryBaseDelay
	}
	if strings.TrimSpace(config.HostName) == "" {
		config.HostName, _ = os.Hostname()
	}
	if config.ProcessID <= 0 {
		config.ProcessID = os.Getpid()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if config.SkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 仅在用户显式配置后用于内网自签名 OTLP 服务。
	}
	return &OTLPExporter{
		config: config,
		client: &http.Client{Transport: transport},
		logger: logger,
	}, nil
}

// Export 编码并发送一个批次，在可重试故障下最多存活配置的重试窗口。
func (exporter *OTLPExporter) Export(ctx context.Context, events []Event) error {
	if exporter == nil || len(events) == 0 {
		return nil
	}
	payload, err := proto.Marshal(exporter.request(events))
	if err != nil {
		return ErrOTLPExport
	}
	startedAt := time.Now()
	for attempt := 0; ; attempt++ {
		retry, retryAfter, err := exporter.send(ctx, payload)
		if err == nil {
			return nil
		}
		if !retry || ctx.Err() != nil {
			return ErrOTLPExport
		}
		remaining := exporter.config.RetryLifetime - time.Since(startedAt)
		if remaining <= 0 {
			return ErrOTLPExport
		}
		delay := retryAfter
		if delay <= 0 {
			delay = exporter.retryDelay(attempt)
		}
		if delay > remaining {
			return ErrOTLPExport
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ErrOTLPExport
		}
	}
}

func (exporter *OTLPExporter) send(ctx context.Context, payload []byte) (bool, time.Duration, error) {
	requestContext, cancel := context.WithTimeout(ctx, exporter.config.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, exporter.config.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, 0, err
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	for name, value := range exporter.config.Headers {
		request.Header.Set(name, value)
	}
	response, err := exporter.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return false, 0, err
		}
		return true, 0, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxOTLPResponseBytes))
	if readErr != nil {
		return true, 0, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout
		return retryable, parseRetryAfter(response.Header.Get("Retry-After"), time.Now()), ErrOTLPExport
	}
	if len(body) == 0 {
		return false, 0, nil
	}
	var result collectorlogsv1.ExportLogsServiceResponse
	if err := proto.Unmarshal(body, &result); err != nil {
		return false, 0, err
	}
	if partial := result.GetPartialSuccess(); partial != nil && (partial.RejectedLogRecords > 0 || partial.ErrorMessage != "") && exporter.logger != nil {
		exporter.logger.Warn("OTLP 审计部分成功", "error_type", "partial_success", "rejected_records", partial.RejectedLogRecords)
	}
	return false, 0, nil
}

func (exporter *OTLPExporter) request(events []Event) *collectorlogsv1.ExportLogsServiceRequest {
	records := make([]*logsv1.LogRecord, 0, len(events))
	for _, event := range events {
		records = append(records, eventLogRecord(event))
	}
	resourceAttributes := []*commonv1.KeyValue{
		stringKeyValue("service.name", "herdr-pal-server"),
		stringKeyValue("service.version", exporter.config.ServiceVersion),
		stringKeyValue("host.name", exporter.config.HostName),
		intKeyValue("process.pid", int64(exporter.config.ProcessID)),
	}
	return &collectorlogsv1.ExportLogsServiceRequest{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource: &resourcev1.Resource{Attributes: resourceAttributes},
		ScopeLogs: []*logsv1.ScopeLogs{{
			Scope:      &commonv1.InstrumentationScope{Name: "github.com/wenxichang/herdr-pal/audit", Version: exporter.config.ServiceVersion},
			LogRecords: records,
		}},
	}}}
}

func eventLogRecord(event Event) *logsv1.LogRecord {
	severity := logsv1.SeverityNumber_SEVERITY_NUMBER_INFO
	severityText := "INFO"
	if event.Outcome == "rate_limited" || event.Outcome == "delivery_failed" {
		severity = logsv1.SeverityNumber_SEVERITY_NUMBER_WARN
		severityText = "WARN"
	}
	attributes := []*commonv1.KeyValue{
		intKeyValue("herdr_pal.audit.schema_version", int64(event.SchemaVersion)),
		stringKeyValue("herdr_pal.audit.event_id", event.EventID),
		stringKeyValue("herdr_pal.audit.principal_id", event.PrincipalID),
		stringKeyValue("herdr_pal.audit.bot_id_hash", event.BotIDHash),
		stringKeyValue("herdr_pal.audit.message_id_hash", event.MessageIDHash),
		stringKeyValue("herdr_pal.audit.request_id_hash", event.RequestIDHash),
		stringKeyValue("herdr_pal.audit.action", event.Action),
		stringKeyValue("herdr_pal.audit.outcome", event.Outcome),
		stringKeyValue("herdr_pal.audit.machine_id", event.MachineID),
		stringKeyValue("herdr_pal.audit.pane_id", event.PaneID),
		stringKeyValue("herdr_pal.audit.session_id_hash", event.SessionIDHash),
		stringKeyValue("herdr_pal.audit.presentation", event.Presentation),
		stringKeyValue("herdr_pal.audit.delivery", event.Delivery),
		intKeyValue("herdr_pal.audit.content_bytes", int64(event.ContentBytes)),
	}
	keys := make([]string, 0, len(event.Attributes))
	for key := range event.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attributes = append(attributes, stringKeyValue("herdr_pal.audit."+key, event.Attributes[key]))
	}
	return &logsv1.LogRecord{
		TimeUnixNano: uint64(event.Timestamp.UnixNano()), ObservedTimeUnixNano: uint64(event.ObservedTimestamp.UnixNano()),
		SeverityNumber: severity, SeverityText: severityText, EventName: event.EventName,
		Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: event.Body}}, Attributes: attributes,
	}
}

func stringKeyValue(key, value string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: key, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: value}}}
}

func intKeyValue(key string, value int64) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: key, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: value}}}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if delay := deadline.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func (exporter *OTLPExporter) retryDelay(attempt int) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	delay := exporter.config.RetryBaseDelay * time.Duration(1<<attempt)
	return time.Duration(float64(delay) * (0.5 + rand.Float64()))
}
