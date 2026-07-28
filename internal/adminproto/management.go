package adminproto

import "time"

// EmptyParams 表示不接受任何参数的管理方法。
type EmptyParams struct{}

// TLSStatus 是不包含证书路径和私钥信息的 TLS 运行状态。
type TLSStatus struct {
	Mode              string    `json:"mode"`
	NotAfter          time.Time `json:"not_after"`
	SHA256Fingerprint string    `json:"sha256_fingerprint"`
}

// WeComStatus 是不包含 Secret、消息和底层响应正文的企业微信状态。
type WeComStatus struct {
	State          string    `json:"state"`
	ChangedAt      time.Time `json:"changed_at"`
	LastErrorType  string    `json:"last_error_type,omitempty"`
	LastErrorCode  int       `json:"last_error_code,omitempty"`
	LastHTTPStatus int       `json:"last_http_status,omitempty"`
}

// CredentialCounts 是按观测时间互斥分类的 Key 数量。
type CredentialCounts struct {
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
	Expired  int `json:"expired"`
}

// ServerStatusResult 是 server.status 返回的安全运行快照。
type ServerStatusResult struct {
	ObservedAt      time.Time        `json:"observed_at"`
	StartedAt       time.Time        `json:"started_at"`
	UptimeMS        int64            `json:"uptime_ms"`
	Version         string           `json:"version"`
	Commit          string           `json:"commit"`
	BuiltAt         string           `json:"built_at"`
	PID             int              `json:"pid"`
	GOOS            string           `json:"os"`
	GOARCH          string           `json:"arch"`
	HPAP            string           `json:"hpap"`
	HPRP            string           `json:"hprp"`
	RelayListen     string           `json:"relay_listen"`
	AdminSocket     string           `json:"admin_socket"`
	TLS             TLSStatus        `json:"tls"`
	WeCom           WeComStatus      `json:"wecom"`
	DebugEnabled    bool             `json:"debug_enabled"`
	BaseLogLevel    string           `json:"base_log_level"`
	PrincipalCount  int              `json:"principal_count"`
	ConnectionCount int              `json:"connection_count"`
	SessionCount    int              `json:"session_count"`
	Credentials     CredentialCounts `json:"credentials"`
}

// ServerDebugResult 返回动态切换后的实际 debug 状态和配置基础级别。
type ServerDebugResult struct {
	DebugEnabled bool   `json:"debug_enabled"`
	BaseLogLevel string `json:"base_log_level"`
}

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

// Implementation 描述 Pal 实现版本和运行平台。
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Connection 是一条不包含 Bearer Key 的 HPRP 连接视图。
type Connection struct {
	ConnectionID     string         `json:"connection_id"`
	CredentialID     uint64         `json:"credential_id"`
	PrincipalID      string         `json:"principal_id"`
	MachineID        string         `json:"machine_id"`
	Implementation   Implementation `json:"implementation"`
	SourceIP         string         `json:"source_ip"`
	ConnectedAt      time.Time      `json:"connected_at"`
	LastHeartbeatAt  time.Time      `json:"last_heartbeat_at"`
	LastSnapshotAt   time.Time      `json:"last_snapshot_at"`
	SnapshotSequence uint64         `json:"snapshot_sequence"`
	SessionCount     int            `json:"session_count"`
	Capabilities     []string       `json:"capabilities"`
	Ready            bool           `json:"ready"`
}

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

// SessionTarget 是 HPRP 命令使用的完整稳定目标。
type SessionTarget struct {
	MachineID string `json:"machine_id"`
	SlotID    string `json:"slot_id"`
	SessionID string `json:"session_id"`
}

// Session 是不包含终端内容的在线 Agent 会话视图。
type Session struct {
	PrincipalID    string        `json:"principal_id"`
	Number         int           `json:"number"`
	Target         SessionTarget `json:"target"`
	DisplayIndex   int           `json:"display_index"`
	Workspace      string        `json:"workspace,omitempty"`
	Tab            string        `json:"tab,omitempty"`
	WorkspaceLabel string        `json:"workspace_label"`
	Agent          string        `json:"agent,omitempty"`
	DisplayAgent   string        `json:"display_agent,omitempty"`
	Pane           string        `json:"pane"`
	Title          string        `json:"title,omitempty"`
	Status         string        `json:"status"`
	StatusLabel    string        `json:"status_label"`
}

// SessionListResult 返回同一观测时间下的一页 Agent 会话。
type SessionListResult struct {
	ObservedAt    time.Time `json:"observed_at"`
	Items         []Session `json:"items"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}
