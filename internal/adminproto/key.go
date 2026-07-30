package adminproto

import (
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

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

// Credential 是共享管理服务安全凭据视图的 HPAP 兼容别名。
type Credential = adminservice.Credential

// KeyIssueResult 是共享管理服务签发结果的 HPAP 兼容别名。
type KeyIssueResult = adminservice.CredentialIssueResult

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

// CredentialMutationResult 是共享管理服务变更结果的 HPAP 兼容别名。
type CredentialMutationResult = adminservice.CredentialMutationResult

// KeyDeleteResult 是共享管理服务删除结果的 HPAP 兼容别名。
type KeyDeleteResult = adminservice.CredentialDeleteResult

// KeySourceListResult 返回当前规范化来源规则。
type KeySourceListResult struct {
	CredentialID uint64   `json:"credential_id"`
	Sources      []string `json:"sources"`
}
