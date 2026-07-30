package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// StderrExporter 把审计事件编码为一行一个事件的 JSON Lines。
type StderrExporter struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewStderrExporter 创建写入指定 stderr 流的批量输出器。
func NewStderrExporter(writer io.Writer) *StderrExporter {
	return &StderrExporter{writer: writer}
}

// Export 原子写入一个 JSON Lines 批次。
func (exporter *StderrExporter) Export(_ context.Context, events []Event) error {
	if exporter == nil || exporter.writer == nil {
		return errors.New("stderr 审计输出无效")
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	written, err := exporter.writer.Write(buffer.Bytes())
	if err == nil && written != buffer.Len() {
		return io.ErrShortWrite
	}
	return err
}
