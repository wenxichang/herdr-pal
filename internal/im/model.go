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
	// SendNotification 发送一段属于 target 的主动通知。
	SendNotification(ctx context.Context, target NotificationTarget, content string) error
}
