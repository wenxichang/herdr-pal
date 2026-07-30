package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestStderrExporterWritesOneJSONEventPerLine(t *testing.T) {
	var output bytes.Buffer
	auditor, err := NewAsyncAuditor(NewStderrExporter(&output), nil, AsyncConfig{MaxBatchEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	auditor.Emit(Event{SchemaVersion: 1, EventID: "one", EventName: EventNameUserInput, Body: "line one\nline two"})
	auditor.Emit(Event{SchemaVersion: 1, EventID: "two", EventName: EventNameTerminalOutput, Body: "terminal"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := auditor.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	count := 0
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid JSON line %q: %v", scanner.Text(), err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("line count = %d, output = %q", count, output.String())
	}
}

func TestStderrExporterRejectsShortWrite(t *testing.T) {
	exporter := NewStderrExporter(shortAuditWriter{})
	if err := exporter.Export(context.Background(), []Event{{EventName: EventNameUserInput}}); err != io.ErrShortWrite {
		t.Fatalf("Export() error = %v, want %v", err, io.ErrShortWrite)
	}
}

type shortAuditWriter struct{}

func (shortAuditWriter) Write(data []byte) (int, error) {
	return len(data) / 2, nil
}

func TestMultiAuditorUsesIndependentOutputs(t *testing.T) {
	first := &collectingAuditor{}
	second := &collectingAuditor{}
	multi := NewMultiAuditor(first, NoopAuditor{}, second)
	multi.Emit(Event{EventID: "shared"})
	if len(first.events) != 1 || len(second.events) != 1 || first.events[0].EventID != second.events[0].EventID {
		t.Fatalf("first = %#v, second = %#v", first.events, second.events)
	}
}

type collectingAuditor struct {
	events []Event
}

func (auditor *collectingAuditor) Emit(event Event)               { auditor.events = append(auditor.events, event) }
func (auditor *collectingAuditor) Shutdown(context.Context) error { return nil }
