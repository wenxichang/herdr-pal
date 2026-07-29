package hprp

import (
	"encoding/json"
	"time"
)

const (
	TypeHelloClient            Type = "hello.client"
	TypeHelloServer            Type = "hello.server"
	TypeSessionSnapshot        Type = "session.snapshot"
	TypeSessionSnapshotResult  Type = "session.snapshot.result"
	TypeCommandExecute         Type = "command.execute"
	TypeCommandResult          Type = "command.result"
	TypeCommandOutput          Type = "command.output"
	TypeNotificationEvent      Type = "notification.event"
	TypeFeatureInvoke          Type = "feature.invoke"
	TypeFeatureResult          Type = "feature.result"
	TypeFeatureEvent           Type = "feature.event"
	TypeFeatureCancel          Type = "feature.cancel"
	TypeFeatureCancelResult    Type = "feature.cancel.result"
	TypeTerminalSnapshotGet    Type = "terminal.snapshot.get"
	TypeTerminalSnapshotResult Type = "terminal.snapshot.result"
	TypeProtocolError          Type = "protocol.error"
)

const (
	CapabilityCommandOutputV1    = "command.output.v1"
	CapabilityFeatureInvokeV1    = "feature.invoke.v1"
	CapabilityTerminalSnapshotV1 = "terminal.snapshot.v1"
	CapabilityTerminalImageV1    = "terminal.image.v1"
)

const (
	ContentTypeText     = "text/plain"
	ContentTypeTerminal = "terminal.snapshot"

	NotificationKindAgentStatusChanged  = "agent.status.changed"
	NotificationKindTargetInvalidated   = "target.invalidated"
	TerminalSnapshotPurposeNotification = "notification"
)

const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusDone    = "done"
	StatusUnknown = "unknown"
)

// Implementation 描述协议实现的软件版本，不参与协议版本判断。
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// FeatureOffer 声明某个用户 Feature 版本及其可协商参数。
type FeatureOffer struct {
	Parameters map[string]json.RawMessage `json:"parameters,omitempty"`
}

// ClientLimits 是 Pal 在 hello 中声明的接收和执行能力。
type ClientLimits struct {
	MaxReceiveMessageBytes int   `json:"max_receive_message_bytes"`
	MaxInflightCommands    int   `json:"max_inflight_commands"`
	MaxInflightFeatures    int   `json:"max_inflight_features"`
	IdempotencyWindowMS    int64 `json:"idempotency_window_ms"`
}

// ServerLimits 是本连接最终生效的服务端资源限制。
type ServerLimits struct {
	MaxMessageBytes       int   `json:"max_message_bytes"`
	MaxSessions           int   `json:"max_sessions"`
	MaxInflightCommands   int   `json:"max_inflight_commands"`
	MaxInflightFeatures   int   `json:"max_inflight_features"`
	MaxOutputBytes        int   `json:"max_output_bytes"`
	MaxTerminalTextBytes  int   `json:"max_terminal_text_bytes,omitempty"`
	MaxTerminalImageBytes int   `json:"max_terminal_image_bytes,omitempty"`
	IdempotencyWindowMS   int64 `json:"idempotency_window_ms"`
}

// HeartbeatConfig 是 hello 后固定的 WebSocket 心跳和空闲超时参数。
type HeartbeatConfig struct {
	PingIntervalMS int64 `json:"ping_interval_ms"`
	IdleTimeoutMS  int64 `json:"idle_timeout_ms"`
}

// ClientHello 是 Pal 在认证 Upgrade 后发送的第一条 HPRP 消息。
type ClientHello struct {
	Implementation Implementation          `json:"implementation"`
	Capabilities   []string                `json:"capabilities"`
	Features       map[string]FeatureOffer `json:"features"`
	Limits         ClientLimits            `json:"limits"`
	Diagnostics    map[string]string       `json:"diagnostics,omitempty"`
}

// ServerHello 返回连接身份、协商结果和最终资源限制。
type ServerHello struct {
	ConnectionID string                  `json:"connection_id"`
	MachineID    string                  `json:"machine_id"`
	Capabilities []string                `json:"capabilities"`
	Features     map[string]FeatureOffer `json:"features"`
	Limits       ServerLimits            `json:"limits"`
	Heartbeat    HeartbeatConfig         `json:"heartbeat"`
}

// Target 是 HPRP 命令和通知使用的稳定会话引用。
type Target struct {
	MachineID string `json:"machine_id"`
	SlotID    string `json:"slot_id"`
	SessionID string `json:"session_id"`
}

// SessionDisplay 是只用于用户展示的会话元数据。
type SessionDisplay struct {
	Index        int    `json:"index"`
	Agent        string `json:"agent,omitempty"`
	DisplayAgent string `json:"display_agent,omitempty"`
	Workspace    string `json:"workspace,omitempty"`
	Tab          string `json:"tab,omitempty"`
	Title        string `json:"title,omitempty"`
}

// Session 是 Pal 当前机器上一个可路由 Agent 会话。
type Session struct {
	SlotID    string         `json:"slot_id"`
	SessionID string         `json:"session_id"`
	Display   SessionDisplay `json:"display"`
	Status    string         `json:"status"`
}

// SessionSnapshot 是当前连接绑定机器的完整权威会话视图。
type SessionSnapshot struct {
	Sequence uint64    `json:"sequence"`
	Sessions []Session `json:"sessions"`
}

// SnapshotResult 确认完整快照是否已经应用。
type SnapshotResult struct {
	Outcome         Outcome `json:"outcome"`
	AppliedSequence uint64  `json:"applied_sequence,omitempty"`
	Error           *Error  `json:"error,omitempty"`
}

// OutputMode 指定单次命令或终端快照的展示形式。
type OutputMode string

const (
	OutputModeText  OutputMode = "txt"
	OutputModeImage OutputMode = "img"
)

// TerminalImage 是终端快照携带的内联图片。
type TerminalImage struct {
	MediaType string `json:"media_type"`
	Encoding  string `json:"encoding"`
	Data      string `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	ColorMode string `json:"color_mode"`
}

// TerminalPage 描述终端快照所在分页。
type TerminalPage struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

// Content 是 HPRP/1 文本或终端快照联合内容。
type Content struct {
	Type       string         `json:"type"`
	Text       string         `json:"text"`
	Mode       OutputMode     `json:"mode,omitempty"`
	Image      *TerminalImage `json:"image,omitempty"`
	Page       *TerminalPage  `json:"page,omitempty"`
	CapturedAt *time.Time     `json:"captured_at,omitempty"`
}

// TextContent 是迁移期间保留的文本内容别名。
type TextContent = Content

// CommandExecute 请求 Pal 在稳定目标上执行一条基础 Agent 交互命令。
type CommandExecute struct {
	IdempotencyKey string      `json:"idempotency_key"`
	Target         Target      `json:"target"`
	Content        TextContent `json:"content"`
	OutputMode     OutputMode  `json:"output_mode,omitempty"`
}

// CommandResult 是基础命令唯一的最终同步结果。
type CommandResult struct {
	Outcome           Outcome  `json:"outcome"`
	Content           *Content `json:"content,omitempty"`
	ReplacementTarget *Target  `json:"replacement_target,omitempty"`
	Error             *Error   `json:"error,omitempty"`
}

// CommandOutput 是命令结果之后的有序终端输出分段。
type CommandOutput struct {
	Target   Target  `json:"target"`
	Sequence uint64  `json:"sequence"`
	Final    bool    `json:"final"`
	Content  Content `json:"content"`
}

// StatusChangeData 描述 Agent 状态变化。
type StatusChangeData struct {
	PreviousStatus string `json:"previous_status"`
	Status         string `json:"status"`
}

// NotificationEvent 是不依赖当前命令的结构化状态事件。
type NotificationEvent struct {
	EventKey         string            `json:"event_key"`
	Sequence         uint64            `json:"sequence"`
	Kind             string            `json:"kind"`
	Target           Target            `json:"target"`
	SnapshotSequence uint64            `json:"snapshot_sequence,omitempty"`
	OccurredAt       time.Time         `json:"occurred_at,omitempty"`
	Data             *StatusChangeData `json:"data,omitempty"`
	Content          Content           `json:"content,omitempty"`
}

// TerminalSnapshotGet 请求 Pal 无副作用读取一次终端快照。
type TerminalSnapshotGet struct {
	Target   Target     `json:"target"`
	Mode     OutputMode `json:"mode"`
	Purpose  string     `json:"purpose"`
	MaxLines int        `json:"max_lines"`
}

// TerminalSnapshotResult 返回终端快照或同次读取的文本降级内容。
type TerminalSnapshotResult struct {
	Outcome         Outcome  `json:"outcome"`
	Target          Target   `json:"target"`
	Content         *Content `json:"content,omitempty"`
	FallbackContent *Content `json:"fallback_content,omitempty"`
	Error           *Error   `json:"error,omitempty"`
}

// FeatureInvoke 发起一个已协商的用户功能，target 和 input 由 Feature Package 定义。
type FeatureInvoke struct {
	Feature        string          `json:"feature"`
	IdempotencyKey string          `json:"idempotency_key"`
	Target         json.RawMessage `json:"target"`
	Input          json.RawMessage `json:"input"`
}

// FeatureResultBody 保存 Feature Package 定义的用户结果和已观察状态。
type FeatureResultBody struct {
	Content       *TextContent    `json:"content,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	ObservedState json.RawMessage `json:"observed_state,omitempty"`
}

// FeatureResult 是一次用户 Feature 调用的唯一最终结果。
type FeatureResult struct {
	Feature string             `json:"feature"`
	Outcome Outcome            `json:"outcome"`
	Result  *FeatureResultBody `json:"result,omitempty"`
	Error   *Error             `json:"error,omitempty"`
}

// FeatureEvent 是 Feature 执行期间的有序非最终事件。
type FeatureEvent struct {
	Feature  string          `json:"feature"`
	Sequence uint64          `json:"sequence"`
	Kind     string          `json:"kind"`
	Content  *TextContent    `json:"content,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// FeatureCancel 请求停止仍在执行的 Feature 调用。
type FeatureCancel struct {
	InvocationID string `json:"invocation_id"`
}

// FeatureCancelResult 表示取消请求是否被接受。
type FeatureCancelResult struct {
	Outcome Outcome `json:"outcome"`
	Error   *Error  `json:"error,omitempty"`
}
