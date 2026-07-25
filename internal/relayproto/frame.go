// Package relayproto 定义 Herdr Pal 客户端与服务端之间的版本化 Relay 协议。
package relayproto

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ProtocolVersion 是当前 Relay 协议版本。
	ProtocolVersion = 2
	// MaxFrameBytes 是单条 WebSocket 文本帧允许的最大字节数。
	MaxFrameBytes = 1 << 20
	// MaxSessionsPerSnapshot 是单台机器可上报的最大会话数。
	MaxSessionsPerSnapshot = 256
	// MaxUserIDBytes 是 userid 的最大 UTF-8 字节数。
	MaxUserIDBytes = 512
	// MaxMachineIDBytes 是 machine_id 的最大 ASCII 字节数。
	MaxMachineIDBytes = 64
	// MaxLabelBytes 是展示字段和稳定本地标识的最大 UTF-8 字节数。
	MaxLabelBytes = 512
)

var (
	// ErrInvalidFrame 表示 Relay 帧结构或字段不合法。
	ErrInvalidFrame = errors.New("Relay 帧无效")
	// ErrProtocolMismatch 表示对端 Relay 协议版本不受支持。
	ErrProtocolMismatch = errors.New("Relay 协议版本不兼容")
	// ErrFrameTooLarge 表示帧超过固定大小限制。
	ErrFrameTooLarge = errors.New("Relay 帧过大")
	// ErrInvalidIdentity 表示 userid 或 machine_id 不合法。
	ErrInvalidIdentity = errors.New("Relay 身份无效")
	// ErrInvalidSnapshot 表示完整会话快照不合法。
	ErrInvalidSnapshot = errors.New("Relay 会话快照无效")
	// ErrInvalidTarget 表示稳定会话目标不合法。
	ErrInvalidTarget = errors.New("Relay 会话目标无效")
	// ErrLimitExceeded 表示协议资源数量或字段长度超过限制。
	ErrLimitExceeded = errors.New("Relay 资源限制已超过")
)

// Type 是 Relay 帧的稳定类型。
type Type string

const (
	TypeClientHello     Type = "client_hello"
	TypeServerHello     Type = "server_hello"
	TypeSessionSnapshot Type = "session_snapshot"
	TypeSelectRequest   Type = "select_request"
	TypeSelectResult    Type = "select_result"
	TypeExecuteRequest  Type = "execute_request"
	TypeExecuteResponse Type = "execute_response"
	TypeExecutePush     Type = "execute_push"
	TypeNotification    Type = "notification"
	TypePing            Type = "ping"
	TypePong            Type = "pong"
	TypeProtocolError   Type = "protocol_error"
)

// ErrorCode 是可跨连接稳定分类的协议错误码。
type ErrorCode string

const (
	CodeProtocolMismatch  ErrorCode = "protocol_mismatch"
	CodeInvalidFrame      ErrorCode = "invalid_frame"
	CodeInvalidIdentity   ErrorCode = "invalid_identity"
	CodeDuplicateClient   ErrorCode = "duplicate_client"
	CodeSnapshotStale     ErrorCode = "snapshot_stale"
	CodeTargetNotFound    ErrorCode = "target_not_found"
	CodeTargetChanged     ErrorCode = "target_changed"
	CodeClientUnavailable ErrorCode = "client_unavailable"
	CodeQueueFull         ErrorCode = "queue_full"
	CodeRequestTimeout    ErrorCode = "request_timeout"
)

// Frame 是一条 Relay WebSocket 文本消息的公共 envelope。
type Frame struct {
	Protocol  int             `json:"protocol"`
	Type      Type            `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// ClientHello 声明客户端用户、机器和程序版本。
type ClientHello struct {
	UserID        string `json:"userid"`
	MachineID     string `json:"machine_id"`
	ClientVersion string `json:"client_version"`
}

// ServerHello 返回连接身份和服务端时间参数。
type ServerHello struct {
	ConnectionID          string `json:"connection_id"`
	HeartbeatIntervalSecs int    `json:"heartbeat_interval_seconds"`
	HeartbeatTimeoutSecs  int    `json:"heartbeat_timeout_seconds"`
	SnapshotIntervalSecs  int    `json:"snapshot_interval_seconds"`
}

// SessionSnapshot 是一台机器在当前连接周期内的完整会话视图。
type SessionSnapshot struct {
	Sequence uint64    `json:"sequence"`
	Sessions []Session `json:"sessions"`
}

// Session 是服务端目录保存的一个本地 Agent 会话。
type Session struct {
	LocalIndex      int    `json:"local_index"`
	PaneID          string `json:"pane_id"`
	TerminalID      string `json:"terminal_id"`
	OccupantHash    string `json:"occupant_hash"`
	AgentSessionRef string `json:"agent_session_ref,omitempty"`
	Agent           string `json:"agent"`
	DisplayAgent    string `json:"display_agent"`
	Title           string `json:"title"`
	Workspace       string `json:"workspace"`
	Tab             string `json:"tab"`
	Status          string `json:"status"`
}

// SessionRef 是服务端和客户端共同复核的稳定目标引用。
type SessionRef struct {
	MachineID    string `json:"machine_id"`
	LocalIndex   int    `json:"local_index"`
	PaneID       string `json:"pane_id"`
	OccupantHash string `json:"occupant_hash"`
}

type SelectRequest struct {
	Target SessionRef `json:"target"`
}

type SelectResult struct {
	OK      bool      `json:"ok"`
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

type ExecuteRequest struct {
	Target    SessionRef `json:"target"`
	MessageID string     `json:"message_id"`
	UserID    string     `json:"userid"`
	Content   string     `json:"content"`
}

type ExecuteResponse struct {
	Content string `json:"content"`
}

type ExecutePush struct {
	Target  SessionRef `json:"target"`
	Content string     `json:"content"`
}

type Notification struct {
	Target  SessionRef `json:"target"`
	Content string     `json:"content"`
}

type Heartbeat struct {
	Nonce string `json:"nonce"`
}

type ProtocolErrorPayload struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Close   bool      `json:"close"`
}

// ProtocolError 保存可写入协议的固定错误码和安全展示信息。
type ProtocolError struct {
	Code    ErrorCode
	Message string
	Close   bool
}

// NewProtocolError 创建不会在 Error 文本中回显 Message 的协议错误。
func NewProtocolError(code ErrorCode, message string, closeConnection bool) *ProtocolError {
	return &ProtocolError{Code: code, Message: message, Close: closeConnection}
}

// Error 返回固定错误码，不回显可能来自外部输入的展示信息。
func (e *ProtocolError) Error() string {
	if e == nil || e.Code == "" {
		return ErrInvalidFrame.Error()
	}
	return fmt.Sprintf("Relay 协议错误: %s", e.Code)
}

// Payload 返回可编码到 protocol_error 帧的负载。
func (e *ProtocolError) Payload() ProtocolErrorPayload {
	if e == nil {
		return ProtocolErrorPayload{Code: CodeInvalidFrame, Close: true}
	}
	return ProtocolErrorPayload{Code: e.Code, Message: e.Message, Close: e.Close}
}
