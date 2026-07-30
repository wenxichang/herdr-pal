package adminservice

import "time"

// Credential 是不包含 Secret 摘要和明文 Key 的机器凭据视图。
type Credential struct {
	CredentialID   uint64     `json:"credential_id"`
	PrincipalID    string     `json:"principal_id"`
	MachineID      string     `json:"machine_id"`
	Status         string     `json:"status"`
	AllowedSources []string   `json:"allowed_sources"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// IssueCredentialInput 是签发机器凭据所需的身份和来源策略。
type IssueCredentialInput struct {
	PrincipalID string
	MachineID   string
	Sources     []string
	ExpiresAt   *time.Time
}

// CredentialIssueResult 只在签发成功时携带一次完整机器 Key。
type CredentialIssueResult struct {
	Token      string     `json:"token"`
	Credential Credential `json:"credential"`
}

// CredentialMutationResult 返回凭据变更和因此撤下的连接数量。
type CredentialMutationResult struct {
	Credential              Credential `json:"credential"`
	DisconnectedConnections int        `json:"disconnected_connections"`
}

// CredentialDeleteResult 返回不可恢复删除的结果。
type CredentialDeleteResult struct {
	CredentialID            uint64 `json:"credential_id"`
	Deleted                 bool   `json:"deleted"`
	DisconnectedConnections int    `json:"disconnected_connections"`
}

// SourceOperation 是来源地址集合的修改方式。
type SourceOperation string

const (
	SourceAdd    SourceOperation = "add"
	SourceRemove SourceOperation = "remove"
	SourceSet    SourceOperation = "set"
)

// Implementation 描述 Pal 的实现版本和平台。
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

// SessionFilter 限定管理面观察的用户和机器。
type SessionFilter struct {
	PrincipalID string
	MachineID   string
}

// SessionTarget 是 HPRP 命令使用的完整稳定目标。
type SessionTarget struct {
	MachineID string `json:"machine_id"`
	SlotID    string `json:"slot_id"`
	SessionID string `json:"session_id"`
}

// Session 是不包含终端正文的在线 Agent 会话视图。
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

// TLSStatus 是不包含证书路径和私钥信息的 TLS 状态。
type TLSStatus struct {
	Mode              string    `json:"mode"`
	NotAfter          time.Time `json:"not_after"`
	SHA256Fingerprint string    `json:"sha256_fingerprint"`
}

// WeComStatus 是不包含 Secret 和消息正文的企业微信状态。
type WeComStatus struct {
	State          string    `json:"state"`
	ChangedAt      time.Time `json:"changed_at"`
	LastErrorType  string    `json:"last_error_type,omitempty"`
	LastErrorCode  int       `json:"last_error_code,omitempty"`
	LastHTTPStatus int       `json:"last_http_status,omitempty"`
}

// CredentialCounts 是按生命周期分类的机器凭据数量。
type CredentialCounts struct {
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
	Expired  int `json:"expired"`
}

// ServerStatus 是不包含敏感配置的运行状态快照。
type ServerStatus struct {
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
	WebAdminListen  string           `json:"web_admin_listen,omitempty"`
	TLS             TLSStatus        `json:"tls"`
	WeCom           WeComStatus      `json:"wecom"`
	DebugEnabled    bool             `json:"debug_enabled"`
	BaseLogLevel    string           `json:"base_log_level"`
	PrincipalCount  int              `json:"principal_count"`
	ConnectionCount int              `json:"connection_count"`
	SessionCount    int              `json:"session_count"`
	Credentials     CredentialCounts `json:"credentials"`
}

// DebugStatus 返回动态日志开关和配置基础级别。
type DebugStatus struct {
	DebugEnabled bool   `json:"debug_enabled"`
	BaseLogLevel string `json:"base_log_level"`
}
