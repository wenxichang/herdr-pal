// Package machinereg 管理企业微信机器自主注册申请及其审批生命周期。
package machinereg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

// Disposition 表示企微自主注册请求的处理结果。
type Disposition string

const (
	// DispositionAutoIssued 表示用户首台机器已直接签发并交付 Key。
	DispositionAutoIssued Disposition = "auto_issued"
	// DispositionPending 表示申请已进入管理员审批队列。
	DispositionPending Disposition = "pending"
	// DispositionAlreadyPending 表示同一机器已有待审批申请。
	DispositionAlreadyPending Disposition = "already_pending"
)

// MaxRejectionReasonBytes 是管理员驳回原因的最大 UTF-8 字节数。
const MaxRejectionReasonBytes = 512

var (
	// ErrInvalidRequest 表示机器注册申请字段或持久化数据无效。
	ErrInvalidRequest = errors.New("机器注册申请无效")
	// ErrRequestNotFound 表示指定的待审批申请不存在。
	ErrRequestNotFound = errors.New("机器注册申请不存在")
	// ErrMachineExists 表示同一用户和机器的凭据已经存在。
	ErrMachineExists = errors.New("用户机器凭据已存在")
	// ErrDeliveryFailed 表示新签发的机器 Key 未能交付给用户。
	ErrDeliveryFailed = errors.New("机器 Key 交付失败")
	// ErrRollbackFailed 表示 Key 交付失败后无法删除已签发凭据。
	ErrRollbackFailed = errors.New("机器凭据回滚失败")
	// ErrCleanupFailed 表示 Key 已交付但待审批申请未能删除。
	ErrCleanupFailed = errors.New("待审批申请清理失败")
	// ErrInsecurePermissions 表示注册申请文件权限过宽。
	ErrInsecurePermissions = errors.New("机器注册申请文件权限不安全")
)

// Request 是只在等待管理员审批期间持久化的机器注册申请。
type Request struct {
	RegistrationID string                  `json:"registration_id"`
	PrincipalID    string                  `json:"principal_id"`
	MachineID      string                  `json:"machine_id"`
	AllowedSources []credential.SourceRule `json:"allowed_sources"`
	RequestedAt    time.Time               `json:"requested_at"`
}

// RegisterInput 是企微用户发起机器注册所需的可信身份和用户参数。
type RegisterInput struct {
	PrincipalID string
	MachineID   string
	Sources     []string
}

// RegisterResult 返回自主注册是直接签发还是等待审批。
type RegisterResult struct {
	Disposition  Disposition
	Request      *Request
	CredentialID uint64
}

// KeyDelivery 是仅在一次交付回调中暴露的机器 Key。
type KeyDelivery struct {
	PrincipalID  string
	MachineID    string
	CredentialID uint64
	Token        string
}

// KeyDeliveryFunc 将一次性机器 Key 交付给已认证用户。
type KeyDeliveryFunc func(context.Context, KeyDelivery) error

// RejectionDelivery 是发送给申请人的驳回通知。
type RejectionDelivery struct {
	PrincipalID    string
	MachineID      string
	RegistrationID string
	Reason         string
}

// RejectionDeliveryFunc 向申请人发送驳回通知。
type RejectionDeliveryFunc func(context.Context, RejectionDelivery) error

// ApprovalResult 返回已批准申请和不敏感的 credential ID。
type ApprovalResult struct {
	Request      Request
	CredentialID uint64
}

// RejectionResult 返回被删除的申请和通知投递状态。
type RejectionResult struct {
	Request          Request
	NotificationSent bool
}

// OperationError 提供不含 Key 和文件路径的失败阶段及关联凭据。
type OperationError struct {
	Kind         error
	Stage        string
	CredentialID uint64
	Cause        error
}

// Error 返回可安全展示的领域错误，不包含底层文件路径或 Key。
func (err *OperationError) Error() string {
	if err == nil {
		return "机器注册操作失败"
	}
	if err.CredentialID != 0 {
		return fmt.Sprintf("%v（credential_id=%d）", err.Kind, err.CredentialID)
	}
	if err.Kind == nil {
		return "机器注册操作失败"
	}
	return err.Kind.Error()
}

// Unwrap 同时暴露领域错误和底层原因，便于 errors.Is/As 诊断。
func (err *OperationError) Unwrap() []error {
	if err == nil {
		return nil
	}
	if err.Cause == nil {
		return []error{err.Kind}
	}
	return []error{err.Kind, err.Cause}
}

// CredentialIDFromError 提取失败操作已持久化的 credential ID。
func CredentialIDFromError(value error) (uint64, bool) {
	var operation *OperationError
	if !errors.As(value, &operation) || operation.CredentialID == 0 {
		return 0, false
	}
	return operation.CredentialID, true
}
