// Package lokiquery 提供受控的 Loki 审计日志查询能力。
package lokiquery

import "time"

// Query 描述管理台允许使用的审计日志过滤条件。
type Query struct {
	PrincipalID string
	MachineID   string
	Keyword     string
	Start       time.Time
	End         time.Time
	Limit       int
	Cursor      string
}

// Entry 是一条从 Loki 审计流中提取的业务日志。
type Entry struct {
	Timestamp     time.Time `json:"timestamp"`
	EventName     string    `json:"event_name"`
	PrincipalID   string    `json:"userid"`
	MachineID     string    `json:"machine_id,omitempty"`
	Agent         string    `json:"agent,omitempty"`
	PaneID        string    `json:"pane_id,omitempty"`
	SessionIDHash string    `json:"session_id_hash,omitempty"`
	Action        string    `json:"action,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	Body          string    `json:"body"`
}

// Page 是按时间倒序返回的一页审计日志。
type Page struct {
	Items      []Entry `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
