// Package audit 定义 herdr-pal-server 的业务审计事件和非阻塞输出接口。
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	// EventNameUserInput 表示一条已完成幂等与限速判断的用户输入。
	EventNameUserInput = "herdr_pal.user_input"
	// EventNameTerminalOutput 表示一次企业微信终端内容投递结果。
	EventNameTerminalOutput = "herdr_pal.terminal_output"
)

var ErrInvalidEvent = errors.New("审计事件无效")

// Event 是 stderr 与 OTLP 共用的版本化业务审计事件。
type Event struct {
	SchemaVersion     int               `json:"schema_version"`
	EventID           string            `json:"event_id"`
	EventName         string            `json:"event_name"`
	Timestamp         time.Time         `json:"timestamp"`
	ObservedTimestamp time.Time         `json:"observed_timestamp"`
	PrincipalID       string            `json:"principal_id"`
	BotIDHash         string            `json:"bot_id_hash,omitempty"`
	MessageIDHash     string            `json:"message_id_hash,omitempty"`
	RequestIDHash     string            `json:"request_id_hash,omitempty"`
	Action            string            `json:"action,omitempty"`
	Outcome           string            `json:"outcome"`
	MachineID         string            `json:"machine_id,omitempty"`
	Agent             string            `json:"agent,omitempty"`
	PaneID            string            `json:"pane_id,omitempty"`
	SessionIDHash     string            `json:"session_id_hash,omitempty"`
	Presentation      string            `json:"presentation,omitempty"`
	Delivery          string            `json:"delivery,omitempty"`
	ContentBytes      int               `json:"content_bytes"`
	Body              string            `json:"body"`
	Attributes        map[string]string `json:"attributes,omitempty"`

	MessageID string `json:"-"`
	RequestID string `json:"-"`
	SessionID string `json:"-"`
}

// Auditor 接收不可变审计事件，并在关闭时尽力刷新已接受事件。
type Auditor interface {
	Emit(Event)
	Shutdown(context.Context) error
}

// PrepareEvent 补齐事件版本、时间、随机 ID 和敏感标识摘要。
func PrepareEvent(event Event, observedAt time.Time, random io.Reader) (Event, error) {
	if event.EventName != EventNameUserInput && event.EventName != EventNameTerminalOutput {
		return Event{}, ErrInvalidEvent
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if random == nil {
		random = rand.Reader
	}
	if event.EventID == "" {
		identifier := make([]byte, 16)
		if _, err := io.ReadFull(random, identifier); err != nil {
			return Event{}, err
		}
		event.EventID = hex.EncodeToString(identifier)
	}
	event.SchemaVersion = 1
	event.ObservedTimestamp = observedAt
	if event.Timestamp.IsZero() {
		event.Timestamp = observedAt
	}
	event.MessageIDHash = HashIdentifier(event.MessageID)
	event.RequestIDHash = HashIdentifier(event.RequestID)
	event.SessionIDHash = HashIdentifier(event.SessionID)
	event.MessageID = ""
	event.RequestID = ""
	event.SessionID = ""
	event.ContentBytes = len([]byte(event.Body))
	return event, nil
}

// HashIdentifier 返回适合审计关联但不暴露原值的短摘要。
func HashIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

// NoopAuditor 丢弃所有事件，用于未启用审计的服务。
type NoopAuditor struct{}

// Emit 丢弃事件。
func (NoopAuditor) Emit(Event) {}

// Shutdown 立即完成关闭。
func (NoopAuditor) Shutdown(context.Context) error { return nil }
