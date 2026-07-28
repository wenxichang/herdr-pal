package adminproto

import "time"

// KeyIssueParams 是签发单机凭据所需的身份、来源和可选到期时间。
type KeyIssueParams struct {
	PrincipalID string   `json:"principal_id"`
	MachineID   string   `json:"machine_id"`
	Sources     []string `json:"sources"`
	ExpiresAt   *string  `json:"expires_at,omitempty"`
}

// KeyListParams 是 Key 列表的有界分页参数。
type KeyListParams struct {
	Limit     int    `json:"limit,omitempty"`
	PageToken string `json:"page_token,omitempty"`
}

// CredentialIDParams 指定一个非零 credential ID。
type CredentialIDParams struct {
	CredentialID uint64 `json:"credential_id"`
}

// KeyDeleteParams 要求调用方显式确认不可恢复的删除操作。
type KeyDeleteParams struct {
	CredentialID uint64 `json:"credential_id"`
	Confirm      bool   `json:"confirm"`
}

// KeySourceMutationParams 指定要添加、删除或替换的来源规则。
type KeySourceMutationParams struct {
	CredentialID uint64   `json:"credential_id"`
	Sources      []string `json:"sources"`
}

// Credential 是不包含 Secret 摘要和明文 Key 的管理面凭据视图。
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

// KeyIssueResult 只在签发成功响应中返回一次完整 Token。
type KeyIssueResult struct {
	Token      string     `json:"token"`
	Credential Credential `json:"credential"`
}

// KeyListResult 返回同一观测时间下的一页凭据。
type KeyListResult struct {
	ObservedAt    time.Time    `json:"observed_at"`
	Items         []Credential `json:"items"`
	NextPageToken string       `json:"next_page_token,omitempty"`
}

// CredentialResult 返回一个不包含 Secret 的凭据视图。
type CredentialResult struct {
	Credential Credential `json:"credential"`
}

// CredentialMutationResult 返回持久化后的凭据和本次撤下的活动连接数。
type CredentialMutationResult struct {
	Credential              Credential `json:"credential"`
	DisconnectedConnections int        `json:"disconnected_connections"`
}

// KeyDeleteResult 返回不可恢复删除的 credential ID 和撤下连接数。
type KeyDeleteResult struct {
	CredentialID            uint64 `json:"credential_id"`
	Deleted                 bool   `json:"deleted"`
	DisconnectedConnections int    `json:"disconnected_connections"`
}

// KeySourceListResult 返回当前规范化来源规则。
type KeySourceListResult struct {
	CredentialID uint64   `json:"credential_id"`
	Sources      []string `json:"sources"`
}
