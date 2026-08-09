package machinereg

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

const principalLockCount = 64

// CredentialManager 提供机器凭据的完整持久化生命周期。
type CredentialManager interface {
	Issue(string, string, []string, *time.Time) (string, credential.Record, error)
	List() []credential.Record
	Show(uint64) (credential.Record, error)
	Enable(uint64) (credential.Record, error)
	Disable(uint64) (credential.Record, error)
	Delete(uint64) (credential.Record, error)
	AddSources(uint64, []string) (credential.Record, error)
	RemoveSources(uint64, []string) (credential.Record, error)
	SetSources(uint64, []string) (credential.Record, error)
}

// Config 指定机器注册协调服务依赖。
type Config struct {
	Credentials CredentialManager
	Requests    *Store
	Auditor     audit.Auditor
	Redactor    *audit.Redactor
	Logger      *slog.Logger
	BotIDHash   string
	Now         func() time.Time
}

// Service 串行化同一用户的注册与人工凭据变更，并协调 Key 交付回滚。
type Service struct {
	credentials CredentialManager
	requests    *Store
	auditor     audit.Auditor
	redactor    *audit.Redactor
	logger      *slog.Logger
	botIDHash   string
	now         func() time.Time
	locks       [principalLockCount]sync.Mutex
}

// New 创建机器注册协调服务。
func New(config Config) (*Service, error) {
	if config.Credentials == nil || config.Requests == nil {
		return nil, ErrInvalidRequest
	}
	if config.Auditor == nil {
		config.Auditor = audit.NoopAuditor{}
	}
	if config.Redactor == nil {
		config.Redactor = audit.NewRedactor(nil)
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{
		credentials: config.Credentials,
		requests:    config.Requests,
		auditor:     config.Auditor,
		redactor:    config.Redactor,
		logger:      config.Logger,
		botIDHash:   config.BotIDHash,
		now:         config.Now,
	}, nil
}

// Register 处理企微自主注册；首台机器直接签发，后续机器进入审批。
func (service *Service) Register(ctx context.Context, input RegisterInput, deliver KeyDeliveryFunc) (RegisterResult, error) {
	if service == nil || credential.ValidatePrincipalID(input.PrincipalID) != nil || hprp.ValidateMachineID(input.MachineID) != nil {
		return RegisterResult{}, ErrInvalidRequest
	}
	rules, err := credential.NormalizeSourceRules(input.Sources)
	if err != nil {
		return RegisterResult{}, &OperationError{Kind: ErrInvalidRequest, Stage: "source_validation", Cause: err}
	}
	lock := service.principalLock(input.PrincipalID)
	lock.Lock()
	defer lock.Unlock()

	records := service.credentials.List()
	if containsMachine(records, input.PrincipalID, input.MachineID) {
		return RegisterResult{}, ErrMachineExists
	}
	if request, exists := service.requests.Find(input.PrincipalID, input.MachineID); exists {
		service.emitRegistration("request", "pending", request, "", 0, "", "", nil, "重复机器注册申请")
		copy := request
		return RegisterResult{Disposition: DispositionAlreadyPending, Request: &copy}, nil
	}
	if hasPrincipal(records, input.PrincipalID) || service.requests.HasPrincipal(input.PrincipalID) {
		request, _, err := service.requests.Create(input.PrincipalID, input.MachineID, rules)
		if err != nil {
			return RegisterResult{}, &OperationError{Kind: ErrInvalidRequest, Stage: "pending_persist", Cause: err}
		}
		service.emitRegistration("request", "pending", request, "", 0, "", "", nil, "机器注册申请等待审批")
		copy := request
		return RegisterResult{Disposition: DispositionPending, Request: &copy}, nil
	}
	if deliver == nil {
		return RegisterResult{}, &OperationError{Kind: ErrInvalidRequest, Stage: "key_delivery"}
	}
	token, record, err := service.credentials.Issue(input.PrincipalID, input.MachineID, input.Sources, nil)
	if err != nil {
		return RegisterResult{}, err
	}
	delivery := KeyDelivery{
		PrincipalID: input.PrincipalID, MachineID: input.MachineID,
		CredentialID: record.CredentialID, Token: token,
	}
	if err := deliver(ctx, delivery); err != nil {
		request := requestFromRecord(record, service.now())
		service.emitRegistration("auto_issue", "delivery_failed", request, "", record.CredentialID, "failed", "key_delivery", err, "首台机器 Key 交付失败")
		if _, rollbackErr := service.credentials.Delete(record.CredentialID); rollbackErr != nil {
			service.emitRegistration("rollback", "rollback_failed", request, "", record.CredentialID, "failed", "credential_delete", rollbackErr, "首台机器凭据回滚失败")
			return RegisterResult{}, &OperationError{Kind: ErrRollbackFailed, Stage: "credential_delete", CredentialID: record.CredentialID, Cause: rollbackErr}
		}
		service.emitRegistration("rollback", "delivery_failed", request, "", record.CredentialID, "rolled_back", "", nil, "首台机器凭据已回滚")
		return RegisterResult{}, &OperationError{Kind: ErrDeliveryFailed, Stage: "key_delivery", Cause: err}
	}
	service.emitRegistration("auto_issue", "delivered", requestFromRecord(record, service.now()), "", record.CredentialID, "delivered", "", nil, "首台机器 Key 已交付")
	return RegisterResult{Disposition: DispositionAutoIssued, CredentialID: record.CredentialID}, nil
}

// ListPending 返回当前待审批申请快照。
func (service *Service) ListPending() []Request {
	if service == nil {
		return nil
	}
	return service.requests.List()
}

// Approve 签发待审批机器 Key，交付失败时回滚凭据并保留申请。
func (service *Service) Approve(ctx context.Context, registrationID, adminUsername string, deliver KeyDeliveryFunc) (ApprovalResult, error) {
	if service == nil || deliver == nil || strings.TrimSpace(adminUsername) == "" {
		return ApprovalResult{}, ErrInvalidRequest
	}
	initial, err := service.requests.Show(registrationID)
	if err != nil {
		return ApprovalResult{}, err
	}
	lock := service.principalLock(initial.PrincipalID)
	lock.Lock()
	defer lock.Unlock()
	request, err := service.requests.Show(registrationID)
	if err != nil {
		return ApprovalResult{}, err
	}
	if containsMachine(service.credentials.List(), request.PrincipalID, request.MachineID) {
		return ApprovalResult{}, ErrMachineExists
	}
	token, record, err := service.credentials.Issue(request.PrincipalID, request.MachineID, sourceStrings(request.AllowedSources), nil)
	if err != nil {
		return ApprovalResult{}, err
	}
	delivery := KeyDelivery{
		PrincipalID: request.PrincipalID, MachineID: request.MachineID,
		CredentialID: record.CredentialID, Token: token,
	}
	if err := deliver(ctx, delivery); err != nil {
		service.emitRegistration("approve", "delivery_failed", request, adminUsername, record.CredentialID, "failed", "key_delivery", err, "审批 Key 交付失败")
		if _, rollbackErr := service.credentials.Delete(record.CredentialID); rollbackErr != nil {
			service.emitRegistration("rollback", "rollback_failed", request, adminUsername, record.CredentialID, "failed", "credential_delete", rollbackErr, "审批凭据回滚失败")
			return ApprovalResult{}, &OperationError{Kind: ErrRollbackFailed, Stage: "credential_delete", CredentialID: record.CredentialID, Cause: rollbackErr}
		}
		service.emitRegistration("rollback", "delivery_failed", request, adminUsername, record.CredentialID, "rolled_back", "", nil, "审批凭据已回滚")
		return ApprovalResult{}, &OperationError{Kind: ErrDeliveryFailed, Stage: "key_delivery", Cause: err}
	}
	if _, err := service.requests.Delete(registrationID); err != nil {
		service.emitRegistration("approve", "delivered", request, adminUsername, record.CredentialID, "delivered", "pending_delete", err, "审批 Key 已交付但申请清理失败")
		return ApprovalResult{}, &OperationError{Kind: ErrCleanupFailed, Stage: "pending_delete", CredentialID: record.CredentialID, Cause: err}
	}
	service.emitRegistration("approve", "delivered", request, adminUsername, record.CredentialID, "delivered", "", nil, "机器注册申请已批准")
	return ApprovalResult{Request: request, CredentialID: record.CredentialID}, nil
}

// Reject 删除待审批申请并尽力通知申请人；通知失败不会恢复申请。
func (service *Service) Reject(ctx context.Context, registrationID, adminUsername, reason string, deliver RejectionDeliveryFunc) (RejectionResult, error) {
	if service == nil || strings.TrimSpace(adminUsername) == "" {
		return RejectionResult{}, ErrInvalidRequest
	}
	reason, err := normalizeRejectionReason(reason)
	if err != nil {
		return RejectionResult{}, err
	}
	initial, err := service.requests.Show(registrationID)
	if err != nil {
		return RejectionResult{}, err
	}
	lock := service.principalLock(initial.PrincipalID)
	lock.Lock()
	defer lock.Unlock()
	request, err := service.requests.Delete(registrationID)
	if err != nil {
		return RejectionResult{}, err
	}
	notificationSent := false
	deliveryStatus := "not_configured"
	var notificationErr error
	if deliver != nil {
		notificationErr = deliver(ctx, RejectionDelivery{
			PrincipalID: request.PrincipalID, MachineID: request.MachineID,
			RegistrationID: request.RegistrationID, Reason: reason,
		})
		if notificationErr == nil {
			notificationSent = true
			deliveryStatus = "delivered"
		} else {
			deliveryStatus = "failed"
			service.logger.Warn("机器注册驳回通知发送失败",
				"principal_hash", audit.HashIdentifier(request.PrincipalID),
				"machine_id", request.MachineID,
				"registration_id", request.RegistrationID,
				"error_type", fmt.Sprintf("%T", notificationErr),
			)
		}
	}
	errorStage := ""
	if notificationErr != nil {
		errorStage = "rejection_delivery"
	}
	service.emitRegistration("reject", "rejected", request, adminUsername, 0, deliveryStatus, errorStage, notificationErr, "机器注册申请已驳回")
	return RejectionResult{Request: request, NotificationSent: notificationSent}, nil
}

// Issue 串行化人工签发，并拒绝绕过同一机器的待审批申请。
func (service *Service) Issue(principalID, machineID string, allowedSources []string, expiresAt *time.Time) (string, credential.Record, error) {
	if service == nil || credential.ValidatePrincipalID(principalID) != nil {
		return "", credential.Record{}, credential.ErrInvalidRecord
	}
	lock := service.principalLock(principalID)
	lock.Lock()
	defer lock.Unlock()
	if _, exists := service.requests.Find(principalID, machineID); exists {
		return "", credential.Record{}, credential.ErrCredentialConflict
	}
	return service.credentials.Issue(principalID, machineID, allowedSources, expiresAt)
}

// List 返回凭据快照。
func (service *Service) List() []credential.Record { return service.credentials.List() }

// Show 返回指定凭据。
func (service *Service) Show(id uint64) (credential.Record, error) {
	return service.credentials.Show(id)
}

// Enable 启用指定凭据。
func (service *Service) Enable(id uint64) (credential.Record, error) {
	return service.withCredentialLock(id, service.credentials.Enable)
}

// Disable 禁用指定凭据。
func (service *Service) Disable(id uint64) (credential.Record, error) {
	return service.withCredentialLock(id, service.credentials.Disable)
}

// Delete 删除指定凭据。
func (service *Service) Delete(id uint64) (credential.Record, error) {
	return service.withCredentialLock(id, service.credentials.Delete)
}

// AddSources 增加凭据来源规则。
func (service *Service) AddSources(id uint64, values []string) (credential.Record, error) {
	return service.withCredentialLock(id, func(id uint64) (credential.Record, error) {
		return service.credentials.AddSources(id, values)
	})
}

// RemoveSources 删除凭据来源规则。
func (service *Service) RemoveSources(id uint64, values []string) (credential.Record, error) {
	return service.withCredentialLock(id, func(id uint64) (credential.Record, error) {
		return service.credentials.RemoveSources(id, values)
	})
}

// SetSources 替换凭据来源规则。
func (service *Service) SetSources(id uint64, values []string) (credential.Record, error) {
	return service.withCredentialLock(id, func(id uint64) (credential.Record, error) {
		return service.credentials.SetSources(id, values)
	})
}

func (service *Service) withCredentialLock(id uint64, operation func(uint64) (credential.Record, error)) (credential.Record, error) {
	if service == nil {
		return credential.Record{}, credential.ErrCredentialNotFound
	}
	initial, err := service.credentials.Show(id)
	if err != nil {
		return credential.Record{}, err
	}
	lock := service.principalLock(initial.PrincipalID)
	lock.Lock()
	defer lock.Unlock()
	current, err := service.credentials.Show(id)
	if err != nil {
		return credential.Record{}, err
	}
	if current.PrincipalID != initial.PrincipalID {
		return credential.Record{}, credential.ErrCredentialConflict
	}
	return operation(id)
}

func (service *Service) principalLock(principalID string) *sync.Mutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(principalID))
	return &service.locks[hasher.Sum32()%principalLockCount]
}

func containsMachine(records []credential.Record, principalID, machineID string) bool {
	for _, record := range records {
		if record.PrincipalID == principalID && record.MachineID == machineID {
			return true
		}
	}
	return false
}

func hasPrincipal(records []credential.Record, principalID string) bool {
	for _, record := range records {
		if record.PrincipalID == principalID {
			return true
		}
	}
	return false
}

func sourceStrings(rules []credential.SourceRule) []string {
	result := make([]string, len(rules))
	for index, rule := range rules {
		result[index] = string(rule)
	}
	return result
}

func requestFromRecord(record credential.Record, requestedAt time.Time) Request {
	return Request{
		PrincipalID: record.PrincipalID, MachineID: record.MachineID,
		AllowedSources: append([]credential.SourceRule(nil), record.AllowedSources...), RequestedAt: requestedAt.UTC(),
	}
}

func normalizeRejectionReason(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || len(value) > MaxRejectionReasonBytes {
		return "", ErrInvalidRequest
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", ErrInvalidRequest
		}
	}
	return value, nil
}

var _ CredentialManager = (*Service)(nil)
