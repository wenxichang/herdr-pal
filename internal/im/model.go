// Package im 定义 Bridge 与消息平台、Relay 传输之间的平台中立接口。
package im

import "context"

// IncomingText 是已通过上游协议校验的单条文本消息。
type IncomingText struct {
	RequestID string
	MessageID string
	BotID     string
	UserID    string
	ChatType  string
	Content   string
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

// NotificationSink 接收携带稳定目标身份的主动通知。
type NotificationSink interface {
	// SendNotification 发送一段属于 target 的主动通知。
	SendNotification(ctx context.Context, target NotificationTarget, content string) error
}
