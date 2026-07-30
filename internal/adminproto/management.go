package adminproto

import (
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

// EmptyParams 表示不接受任何参数的管理方法。
type EmptyParams struct{}

// TLSStatus 是共享管理服务 TLS 状态的 HPAP 兼容别名。
type TLSStatus = adminservice.TLSStatus

// WeComStatus 是共享管理服务企业微信状态的 HPAP 兼容别名。
type WeComStatus = adminservice.WeComStatus

// CredentialCounts 是共享管理服务凭据统计的 HPAP 兼容别名。
type CredentialCounts = adminservice.CredentialCounts

// ServerStatusResult 是共享管理服务运行状态的 HPAP 兼容别名。
type ServerStatusResult = adminservice.ServerStatus

// ServerDebugResult 是共享管理服务动态日志状态的 HPAP 兼容别名。
type ServerDebugResult = adminservice.DebugStatus

// ServerStopResult 确认服务端将在当前响应写出后开始停止。
type ServerStopResult struct {
	Stopping bool `json:"stopping"`
}

// ConnectionListParams 是 HPRP 连接列表的分页参数。
type ConnectionListParams struct {
	Limit     int    `json:"limit,omitempty"`
	PageToken string `json:"page_token,omitempty"`
}

// ConnectionIDParams 指定一条完整 connection ID。
type ConnectionIDParams struct {
	ConnectionID string `json:"connection_id"`
}

// Implementation 是共享管理服务 Pal 实现视图的 HPAP 兼容别名。
type Implementation = adminservice.Implementation

// Connection 是共享管理服务 HPRP 连接视图的 HPAP 兼容别名。
type Connection = adminservice.Connection

// ConnectionListResult 返回同一观测时间下的一页 HPRP 连接。
type ConnectionListResult struct {
	ObservedAt    time.Time    `json:"observed_at"`
	Items         []Connection `json:"items"`
	NextPageToken string       `json:"next_page_token,omitempty"`
}

// ConnectionResult 返回单条 HPRP 连接快照。
type ConnectionResult struct {
	ObservedAt time.Time  `json:"observed_at"`
	Connection Connection `json:"connection"`
}

// ConnectionDisconnectResult 确认指定活动连接已被撤下。
type ConnectionDisconnectResult struct {
	ObservedAt   time.Time `json:"observed_at"`
	ConnectionID string    `json:"connection_id"`
	Disconnected bool      `json:"disconnected"`
}

// SessionListParams 是在线 Agent 会话的过滤和分页参数。
type SessionListParams struct {
	PrincipalID string `json:"principal_id,omitempty"`
	MachineID   string `json:"machine_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	PageToken   string `json:"page_token,omitempty"`
}

// SessionTarget 是共享管理服务会话目标的 HPAP 兼容别名。
type SessionTarget = adminservice.SessionTarget

// Session 是共享管理服务在线 Agent 会话视图的 HPAP 兼容别名。
type Session = adminservice.Session

// SessionListResult 返回同一观测时间下的一页 Agent 会话。
type SessionListResult struct {
	ObservedAt    time.Time `json:"observed_at"`
	Items         []Session `json:"items"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}
