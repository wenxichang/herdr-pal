// Package im 定义 Bridge 与消息平台、Relay 传输之间的平台中立接口。
package im

import (
	"context"
	"time"
)

// OutputMode 指定一次终端输出使用纯文本或图片。
type OutputMode string

const (
	OutputModeText  OutputMode = "txt"
	OutputModeImage OutputMode = "img"
)

// IncomingText 是已通过上游协议校验的单条文本消息。
type IncomingText struct {
	RequestID  string
	MessageID  string
	BotID      string
	UserID     string
	ChatType   string
	Content    string
	OutputMode OutputMode
}

// TerminalImage 是平台中立的终端图片数据。
type TerminalImage struct {
	MediaType string
	Data      []byte
	Width     int
	Height    int
	ColorMode string
}

// TerminalPage 描述终端内容的当前分页位置。
type TerminalPage struct {
	Current int
	Total   int
}

// TerminalContent 同时保存展示内容和用于审计、降级的同页文本。
type TerminalContent struct {
	Mode       OutputMode
	Text       string
	Image      *TerminalImage
	Page       *TerminalPage
	CapturedAt time.Time
}

// NotificationTarget 是主动通知关联的稳定 Agent 目标元数据。
type NotificationTarget struct {
	PaneID       string `json:"pane_id"`
	OccupantHash string `json:"occupant_hash"`
	Agent        string `json:"agent"`
	DisplayAgent string `json:"display_agent"`
	Title        string `json:"title"`
}

// NotificationKind 描述主动通知的业务事件类型。
type NotificationKind string

const (
	// NotificationKindAgentStatusChanged 表示 Agent 状态发生了值得通知的变化。
	NotificationKindAgentStatusChanged NotificationKind = "agent.status.changed"
	// NotificationKindTargetInvalidated 表示原 Agent occupant 已不再是可操作目标。
	NotificationKindTargetInvalidated NotificationKind = "target.invalidated"
)

// NotificationEvent 是 Pal 上报给消息侧的轻量事件，不携带终端快照。
type NotificationEvent struct {
	Kind           NotificationKind
	PreviousStatus string
	Status         string
	OccurredAt     time.Time
}

// ReplySink 接收一条命令的首段回复和后续分段。
type ReplySink interface {
	// RespondMarkdown 使用上游请求标识发送首段 Markdown 回复。
	RespondMarkdown(ctx context.Context, requestID, content string) error
	// SendMarkdown 发送同一命令产生的后续 Markdown 分段。
	SendMarkdown(ctx context.Context, content string) error
}

// TerminalReplySink 接收结构化终端首段回复和后续分段。
type TerminalReplySink interface {
	// RespondTerminal 使用上游请求标识发送结构化终端首段回复。
	RespondTerminal(ctx context.Context, requestID string, content TerminalContent) error
	// SendTerminal 发送同一命令产生的后续结构化终端分段。
	SendTerminal(ctx context.Context, content TerminalContent) error
}

// NotificationSink 接收携带稳定目标身份的主动通知。
type NotificationSink interface {
	// SendNotification 发送一条属于 target 的主动事件。
	SendNotification(ctx context.Context, target NotificationTarget, event NotificationEvent) error
}
