package serverapp

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/config"
)

func newBusinessAuditor(configured config.AuditConfig, stderr io.Writer, logger *slog.Logger, botSecret, serviceVersion string) (audit.Auditor, *audit.Redactor, error) {
	credentialValues := make([]string, 0, len(configured.Headers)+1)
	credentialValues = append(credentialValues, botSecret)
	for _, value := range configured.Headers {
		credentialValues = append(credentialValues, value)
	}
	redactor := audit.NewRedactor(credentialValues)
	outputs := make([]audit.Auditor, 0, 2)
	if configured.Type == "otlp" {
		exporter, err := audit.NewOTLPExporter(audit.OTLPConfig{
			Endpoint: configured.Endpoint, Headers: configured.Headers, SkipVerify: configured.SkipVerify,
			ServiceVersion: serviceVersion,
		}, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("创建 OTLP 审计输出: %w", err)
		}
		output, err := audit.NewAsyncAuditor(exporter, logger, audit.AsyncConfig{})
		if err != nil {
			return nil, nil, fmt.Errorf("创建 OTLP 审计队列: %w", err)
		}
		outputs = append(outputs, output)
	}
	if configured.Stderr {
		output, err := audit.NewAsyncAuditor(audit.NewStderrExporter(stderr), logger, audit.AsyncConfig{})
		if err != nil {
			return nil, nil, fmt.Errorf("创建 stderr 审计队列: %w", err)
		}
		outputs = append(outputs, output)
	}
	return audit.NewMultiAuditor(outputs...), redactor, nil
}
