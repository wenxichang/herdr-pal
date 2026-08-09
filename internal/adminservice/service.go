package adminservice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/server"
)

// CredentialManager 提供机器凭据的持久化生命周期管理。
type CredentialManager interface {
	Issue(principalID, machineID string, allowedSources []string, expiresAt *time.Time) (string, credential.Record, error)
	List() []credential.Record
	Show(credentialID uint64) (credential.Record, error)
	Enable(credentialID uint64) (credential.Record, error)
	Disable(credentialID uint64) (credential.Record, error)
	Delete(credentialID uint64) (credential.Record, error)
	AddSources(credentialID uint64, values []string) (credential.Record, error)
	RemoveSources(credentialID uint64, values []string) (credential.Record, error)
	SetSources(credentialID uint64, values []string) (credential.Record, error)
}

// RegistrationManager 提供待审批注册申请的查询、批准和驳回能力。
type RegistrationManager interface {
	ListPending() []machinereg.Request
	Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error)
	Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error)
}

// ConnectionManager 提供 HPRP 连接查询和撤下能力。
type ConnectionManager interface {
	Connections() []server.ConnectionView
	Connection(connectionID string) (server.ConnectionView, bool)
	DisconnectConnection(connectionID, reason string) bool
	DisconnectCredential(credentialID uint64, reason string) int
	RevalidateCredentialSource(credentialID uint64, rules []credential.SourceRule, reason string) int
}

// SessionInspector 提供不读取终端内容的 Agent 会话快照。
type SessionInspector interface {
	ManagementSessions(server.SessionFilter) []server.SessionView
}

// RuntimeController 提供安全运行状态和动态进程控制。
type RuntimeController interface {
	Status() ServerStatus
	EnableDebug()
	DisableDebug()
	RequestStop() bool
}

// Config 指定共享管理服务的运行依赖。
type Config struct {
	Credentials       CredentialManager
	Connections       ConnectionManager
	Sessions          SessionInspector
	Runtime           RuntimeController
	Registrations     RegistrationManager
	KeyDelivery       machinereg.KeyDeliveryFunc
	RejectionDelivery machinereg.RejectionDeliveryFunc
	Now               func() time.Time
}

// Service 实施 HPAP 与 Web 入口共用的管理规则。
type Service struct {
	credentials       CredentialManager
	connections       ConnectionManager
	sessions          SessionInspector
	runtime           RuntimeController
	registrations     RegistrationManager
	keyDelivery       machinereg.KeyDeliveryFunc
	rejectionDelivery machinereg.RejectionDeliveryFunc
	now               func() time.Time
	stopping          atomic.Bool
}

// New 创建共享管理服务。
func New(config Config) (*Service, error) {
	if config.Credentials == nil || config.Connections == nil || config.Sessions == nil || config.Runtime == nil ||
		config.Registrations == nil || config.KeyDelivery == nil || config.RejectionDelivery == nil {
		return nil, newError(CodeInternal, "管理服务依赖无效", nil)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{
		credentials:       config.Credentials,
		connections:       config.Connections,
		sessions:          config.Sessions,
		runtime:           config.Runtime,
		registrations:     config.Registrations,
		keyDelivery:       config.KeyDelivery,
		rejectionDelivery: config.RejectionDelivery,
		now:               config.Now,
	}, nil
}

// ListRegistrations 返回稳定排序的待审批机器注册申请。
func (service *Service) ListRegistrations() []Registration {
	if service == nil {
		return nil
	}
	requests := service.registrations.ListPending()
	result := make([]Registration, 0, len(requests))
	for _, request := range requests {
		result = append(result, registrationView(request))
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].RequestedAt.Equal(result[right].RequestedAt) {
			return result[left].RegistrationID < result[right].RegistrationID
		}
		return result[left].RequestedAt.Before(result[right].RequestedAt)
	})
	return result
}

// ApproveRegistration 批准待审批申请并仅返回不敏感的 credential ID。
func (service *Service) ApproveRegistration(ctx context.Context, registrationID, adminUsername string) (RegistrationApprovalResult, error) {
	if service == nil || strings.TrimSpace(registrationID) == "" || strings.TrimSpace(adminUsername) == "" {
		return RegistrationApprovalResult{}, newError(CodeInvalidArgument, "注册申请或管理员身份无效", nil)
	}
	result, err := service.registrations.Approve(ctx, registrationID, adminUsername, service.keyDelivery)
	if err != nil {
		return RegistrationApprovalResult{}, mapRegistrationError(err)
	}
	return RegistrationApprovalResult{
		RegistrationID: result.Request.RegistrationID,
		CredentialID:   result.CredentialID,
		Approved:       true,
	}, nil
}

// RejectRegistration 驳回并删除待审批申请；通知失败不改变已生效决定。
func (service *Service) RejectRegistration(ctx context.Context, registrationID, adminUsername, reason string) (RegistrationRejectionResult, error) {
	if service == nil || strings.TrimSpace(registrationID) == "" || strings.TrimSpace(adminUsername) == "" {
		return RegistrationRejectionResult{}, newError(CodeInvalidArgument, "注册申请或管理员身份无效", nil)
	}
	result, err := service.registrations.Reject(ctx, registrationID, adminUsername, reason, service.rejectionDelivery)
	if err != nil {
		return RegistrationRejectionResult{}, mapRegistrationError(err)
	}
	return RegistrationRejectionResult{
		RegistrationID:   result.Request.RegistrationID,
		Rejected:         true,
		NotificationSent: result.NotificationSent,
	}, nil
}

// ObservedAt 返回管理适配器生成同一响应快照时使用的 UTC 时间。
func (service *Service) ObservedAt() time.Time {
	if service == nil || service.now == nil {
		return time.Now().UTC()
	}
	return service.now().UTC()
}

// IssueCredential 持久化签发一条机器凭据并返回一次明文 Key。
func (service *Service) IssueCredential(input IssueCredentialInput) (CredentialIssueResult, error) {
	if service == nil {
		return CredentialIssueResult{}, newError(CodeInternal, "管理服务不可用", nil)
	}
	token, record, err := service.credentials.Issue(input.PrincipalID, input.MachineID, input.Sources, input.ExpiresAt)
	if err != nil {
		return CredentialIssueResult{}, mapCredentialError(err)
	}
	return CredentialIssueResult{Token: token, Credential: credentialView(record)}, nil
}

// ListCredentials 返回按 credential ID 排序的安全凭据快照。
func (service *Service) ListCredentials() []Credential {
	if service == nil {
		return nil
	}
	records := service.credentials.List()
	result := make([]Credential, 0, len(records))
	for _, record := range records {
		result = append(result, credentialView(record))
	}
	sort.Slice(result, func(left, right int) bool { return result[left].CredentialID < result[right].CredentialID })
	return result
}

// ShowCredential 返回指定凭据的安全视图。
func (service *Service) ShowCredential(id uint64) (Credential, error) {
	if service == nil || id == 0 {
		return Credential{}, newError(CodeInvalidArgument, "credential_id 无效", nil)
	}
	record, err := service.credentials.Show(id)
	if err != nil {
		return Credential{}, mapCredentialError(err)
	}
	return credentialView(record), nil
}

// SetCredentialEnabled 持久化启禁用状态，并在禁用成功后撤下连接。
func (service *Service) SetCredentialEnabled(id uint64, enabled bool) (CredentialMutationResult, error) {
	if service == nil || id == 0 {
		return CredentialMutationResult{}, newError(CodeInvalidArgument, "credential_id 无效", nil)
	}
	var (
		record credential.Record
		err    error
	)
	if enabled {
		record, err = service.credentials.Enable(id)
	} else {
		record, err = service.credentials.Disable(id)
	}
	if err != nil {
		return CredentialMutationResult{}, mapCredentialError(err)
	}
	disconnected := 0
	if !enabled {
		disconnected = service.connections.DisconnectCredential(record.CredentialID, "credential disabled")
	}
	return CredentialMutationResult{Credential: credentialView(record), DisconnectedConnections: disconnected}, nil
}

// DeleteCredential 原子删除凭据，并在成功后撤下现有连接。
func (service *Service) DeleteCredential(id uint64) (CredentialDeleteResult, error) {
	if service == nil || id == 0 {
		return CredentialDeleteResult{}, newError(CodeInvalidArgument, "credential_id 无效", nil)
	}
	record, err := service.credentials.Delete(id)
	if err != nil {
		return CredentialDeleteResult{}, mapCredentialError(err)
	}
	disconnected := service.connections.DisconnectCredential(record.CredentialID, "credential deleted")
	return CredentialDeleteResult{CredentialID: record.CredentialID, Deleted: true, DisconnectedConnections: disconnected}, nil
}

// ListSources 返回指定凭据的规范化来源地址规则。
func (service *Service) ListSources(id uint64) ([]string, error) {
	if service == nil || id == 0 {
		return nil, newError(CodeInvalidArgument, "credential_id 无效", nil)
	}
	record, err := service.credentials.Show(id)
	if err != nil {
		return nil, mapCredentialError(err)
	}
	return sourceStrings(record.AllowedSources), nil
}

// MutateSources 持久化来源地址变更，并在收紧规则后复核连接。
func (service *Service) MutateSources(id uint64, operation SourceOperation, values []string) (CredentialMutationResult, error) {
	if service == nil || id == 0 {
		return CredentialMutationResult{}, newError(CodeInvalidArgument, "credential_id 无效", nil)
	}
	if len(values) == 0 {
		return CredentialMutationResult{}, newError(CodeSourceRequired, "Key 至少需要一个来源规则", credential.ErrSourceRequired)
	}
	var (
		record credential.Record
		err    error
	)
	switch operation {
	case SourceAdd:
		record, err = service.credentials.AddSources(id, values)
	case SourceRemove:
		record, err = service.credentials.RemoveSources(id, values)
	case SourceSet:
		record, err = service.credentials.SetSources(id, values)
	default:
		return CredentialMutationResult{}, newError(CodeInvalidArgument, "来源地址操作无效", nil)
	}
	if err != nil {
		return CredentialMutationResult{}, mapCredentialError(err)
	}
	disconnected := 0
	if operation != SourceAdd {
		disconnected = service.connections.RevalidateCredentialSource(record.CredentialID, record.AllowedSources, "credential source policy changed")
	}
	return CredentialMutationResult{Credential: credentialView(record), DisconnectedConnections: disconnected}, nil
}

// ListConnections 返回稳定排序的当前 HPRP 连接快照。
func (service *Service) ListConnections() []Connection {
	if service == nil {
		return nil
	}
	views := service.connections.Connections()
	result := make([]Connection, 0, len(views))
	for _, view := range views {
		result = append(result, connectionView(view))
	}
	sort.Slice(result, func(left, right int) bool {
		return connectionSortKey(result[left]) < connectionSortKey(result[right])
	})
	return result
}

// ShowConnection 返回指定 HPRP 连接快照。
func (service *Service) ShowConnection(id string) (Connection, error) {
	id = strings.TrimSpace(id)
	if service == nil || id == "" || len(id) > 256 {
		return Connection{}, newError(CodeInvalidArgument, "connection_id 无效", nil)
	}
	view, exists := service.connections.Connection(id)
	if !exists {
		return Connection{}, newError(CodeConnectionNotFound, "连接不存在", nil)
	}
	return connectionView(view), nil
}

// DisconnectConnection 只撤下当前连接，不修改机器凭据状态。
func (service *Service) DisconnectConnection(id string) error {
	id = strings.TrimSpace(id)
	if service == nil || id == "" || len(id) > 256 {
		return newError(CodeInvalidArgument, "connection_id 无效", nil)
	}
	if !service.connections.DisconnectConnection(id, "admin request") {
		return newError(CodeConnectionNotFound, "连接不存在", nil)
	}
	return nil
}

// ListSessions 返回不改变用户选择和分页状态的 Agent 会话快照。
func (service *Service) ListSessions(filter SessionFilter) []Session {
	if service == nil {
		return nil
	}
	views := service.sessions.ManagementSessions(server.SessionFilter{PrincipalID: filter.PrincipalID, MachineID: filter.MachineID})
	result := make([]Session, 0, len(views))
	for _, view := range views {
		result = append(result, sessionView(view))
	}
	sort.Slice(result, func(left, right int) bool { return sessionSortKey(result[left]) < sessionSortKey(result[right]) })
	return result
}

// Status 返回当前 Server 的安全运行状态。
func (service *Service) Status() ServerStatus {
	if service == nil {
		return ServerStatus{}
	}
	return service.runtime.Status()
}

// SetDebug 动态启用或恢复配置基础日志级别。
func (service *Service) SetDebug(enabled bool) DebugStatus {
	if service == nil {
		return DebugStatus{}
	}
	if enabled {
		service.runtime.EnableDebug()
	} else {
		service.runtime.DisableDebug()
	}
	status := service.runtime.Status()
	return DebugStatus{DebugEnabled: status.DebugEnabled, BaseLogLevel: status.BaseLogLevel}
}

// StopAction 让传输适配器在成功写出响应后提交停止请求。
type StopAction struct {
	service *Service
	once    sync.Once
}

// PrepareStop 原子预留一次 Server 停止动作。
func (service *Service) PrepareStop() (*StopAction, error) {
	if service == nil {
		return nil, newError(CodeInternal, "管理服务不可用", nil)
	}
	if !service.stopping.CompareAndSwap(false, true) {
		return nil, newError(CodeServerBusy, "服务端已经开始停止", nil)
	}
	return &StopAction{service: service}, nil
}

// Commit 提交已经预留的停止请求。
func (action *StopAction) Commit() {
	if action == nil || action.service == nil {
		return
	}
	action.once.Do(func() { action.service.runtime.RequestStop() })
}

// Rollback 释放尚未提交的停止预留。
func (action *StopAction) Rollback() {
	if action == nil || action.service == nil {
		return
	}
	action.once.Do(func() { action.service.stopping.Store(false) })
}

func mapCredentialError(err error) error {
	switch {
	case errors.Is(err, credential.ErrInvalidRecord):
		return newError(CodeInvalidArgument, "机器凭据参数无效", err)
	case errors.Is(err, credential.ErrCredentialNotFound):
		return newError(CodeCredentialNotFound, "Key 不存在", err)
	case errors.Is(err, credential.ErrCredentialConflict):
		return newError(CodeCredentialConflict, "该用户和机器已存在凭据", err)
	case errors.Is(err, credential.ErrSourceRequired):
		return newError(CodeSourceRequired, "Key 至少需要一个来源规则", err)
	case errors.Is(err, credential.ErrSourceInvalid):
		return newError(CodeSourceInvalid, "Key 来源规则无效", err)
	case errors.Is(err, credential.ErrCredentialIDExhausted):
		return newError(CodeServerBusy, "Key ID 已耗尽", err)
	default:
		return newError(CodeInternal, "机器凭据操作失败", err)
	}
}

func mapRegistrationError(err error) error {
	credentialID, hasCredentialID := machinereg.CredentialIDFromError(err)
	switch {
	case errors.Is(err, machinereg.ErrRequestNotFound):
		return newError(CodeRegistrationNotFound, "机器注册申请不存在", err)
	case errors.Is(err, machinereg.ErrMachineExists), errors.Is(err, credential.ErrCredentialConflict):
		return newError(CodeRegistrationConflict, "该用户和机器已存在凭据", err)
	case errors.Is(err, machinereg.ErrDeliveryFailed):
		return newError(CodeRegistrationDeliveryFailed, "机器 Key 交付失败，申请仍等待审批", err)
	case errors.Is(err, machinereg.ErrRollbackFailed):
		message := "机器 Key 交付失败且凭据回滚失败"
		if hasCredentialID {
			message = fmt.Sprintf("%s（credential_id=%d）", message, credentialID)
		}
		return newError(CodeRegistrationRollbackFailed, message, err)
	case errors.Is(err, machinereg.ErrCleanupFailed):
		message := "机器 Key 已交付但待审批申请清理失败"
		if hasCredentialID {
			message = fmt.Sprintf("%s（credential_id=%d）", message, credentialID)
		}
		return newError(CodeRegistrationCleanupFailed, message, err)
	case errors.Is(err, machinereg.ErrInvalidRequest):
		return newError(CodeInvalidArgument, "机器注册审批参数无效", err)
	default:
		return newError(CodeInternal, "机器注册审批失败", err)
	}
}

func registrationView(request machinereg.Request) Registration {
	return Registration{
		RegistrationID: request.RegistrationID,
		PrincipalID:    request.PrincipalID,
		MachineID:      request.MachineID,
		AllowedSources: sourceStrings(request.AllowedSources),
		RequestedAt:    request.RequestedAt.UTC(),
	}
}

func credentialView(record credential.Record) Credential {
	view := Credential{
		CredentialID: record.CredentialID, PrincipalID: record.PrincipalID, MachineID: record.MachineID,
		Status: string(record.Status), AllowedSources: sourceStrings(record.AllowedSources),
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
	if record.ExpiresAt != nil {
		expiresAt := record.ExpiresAt.UTC()
		view.ExpiresAt = &expiresAt
	}
	return view
}

func sourceStrings(rules []credential.SourceRule) []string {
	result := make([]string, len(rules))
	for index, rule := range rules {
		result[index] = string(rule)
	}
	return result
}

func connectionView(view server.ConnectionView) Connection {
	return Connection{
		ConnectionID: view.ConnectionID, CredentialID: view.CredentialID, PrincipalID: view.PrincipalID, MachineID: view.MachineID,
		Implementation: Implementation{Name: view.Implementation.Name, Version: view.Implementation.Version, OS: view.Implementation.OS, Arch: view.Implementation.Arch},
		SourceIP:       view.SourceIP, ConnectedAt: view.ConnectedAt.UTC(), LastHeartbeatAt: view.LastHeartbeatAt.UTC(),
		LastSnapshotAt: view.LastSnapshotAt.UTC(), SnapshotSequence: view.SnapshotSequence, SessionCount: view.SessionCount,
		Capabilities: append([]string(nil), view.Capabilities...), Ready: view.Ready,
	}
}

func connectionSortKey(view Connection) string {
	return view.PrincipalID + "\x00" + view.MachineID + "\x00" + view.ConnectionID
}

func sessionView(view server.SessionView) Session {
	return Session{
		PrincipalID: view.PrincipalID, Number: view.Number,
		Target:       SessionTarget{MachineID: view.Target.MachineID, SlotID: view.Target.SlotID, SessionID: view.Target.SessionID},
		DisplayIndex: view.Session.Display.Index, Workspace: view.Session.Display.Workspace, Tab: view.Session.Display.Tab,
		WorkspaceLabel: view.WorkspaceLabel, Agent: view.Session.Display.Agent, DisplayAgent: view.Session.Display.DisplayAgent,
		Pane: view.Session.SlotID, Title: view.Session.Display.Title, Status: view.Session.Status, StatusLabel: view.StatusLabel,
	}
}

func sessionSortKey(view Session) string {
	return fmt.Sprintf("%s\x00%020d\x00%s\x00%s\x00%s", view.PrincipalID, view.Number, view.Target.MachineID, view.Target.SlotID, view.Target.SessionID)
}
