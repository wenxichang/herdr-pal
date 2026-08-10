package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
)

var (
	// ErrInvalidRegistrationApprovalDependency 表示注册审批协调器缺少必要依赖。
	ErrInvalidRegistrationApprovalDependency = errors.New("注册审批协调器依赖无效")
	// ErrRegistrationApprovalUnauthorized 表示发送者不属于配置的注册审批管理员。
	ErrRegistrationApprovalUnauthorized = errors.New("当前用户不是注册审批管理员")
	// ErrRegistrationApprovalSnapshotMissing 表示管理员尚未取得待审批列表快照。
	ErrRegistrationApprovalSnapshotMissing = errors.New("尚未获取注册审批列表，请先执行 /ls-reg")
	// ErrRegistrationApprovalSnapshotChanged 表示管理员快照中的所选位置已不再对应同一申请。
	ErrRegistrationApprovalSnapshotChanged = errors.New("注册审批列表已变化，本次未执行")
	// ErrRegistrationApprovalInvalidIndexes 表示协调器收到无效或重复的临时编号。
	ErrRegistrationApprovalInvalidIndexes = errors.New("注册审批编号无效")
)

const (
	registrationApprovalSnapshotReminder = "列表快照已失效，请重新执行 /ls-reg 核实当前条目顺序。"
	maxRegistrationApprovalBatch         = 10
)

// RegistrationApprovalManager 提供注册审批协调器需要的稳定申请列表和单项决策能力。
type RegistrationApprovalManager interface {
	ListPending() []machinereg.Request
	Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error)
	Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error)
}

// RegistrationApprovalHandler 是 ConversationRouter 使用的企业微信注册审批边界。
type RegistrationApprovalHandler interface {
	IsAdmin(string) bool
	NotifyPending(context.Context, machinereg.Request) error
	PrepareList(string) (RegistrationApprovalList, error)
	CommitList(string, RegistrationApprovalList) error
	Approve(context.Context, string, []int) (string, error)
	Reject(context.Context, string, []int) (string, error)
	Invalidate(string) error
}

// RegistrationApprovalCoordinatorConfig 定义企业微信注册审批协调器的依赖。
type RegistrationApprovalCoordinatorConfig struct {
	AdminIDs          []string
	Registrations     RegistrationApprovalManager
	Gateway           WeComGateway
	KeyDelivery       machinereg.KeyDeliveryFunc
	RejectionDelivery machinereg.RejectionDeliveryFunc
	Logger            *slog.Logger
}

// RegistrationApprovalCoordinator 管理管理员轮转通知和每位管理员的临时编号快照。
type RegistrationApprovalCoordinator struct {
	adminIDs          []string
	adminSet          map[string]struct{}
	registrations     RegistrationApprovalManager
	gateway           WeComGateway
	keyDelivery       machinereg.KeyDeliveryFunc
	rejectionDelivery machinereg.RejectionDeliveryFunc
	logger            *slog.Logger

	mu        sync.Mutex
	nextAdmin int
	snapshots map[string][]string
}

type registrationApprovalSelection struct {
	index          int
	registrationID string
	machineID      string
}

// RegistrationApprovalList 是尚未提交给管理员的待审批列表候选快照。
type RegistrationApprovalList struct {
	adminID         string
	content         string
	registrationIDs []string
}

// NewRegistrationApprovalCoordinator 创建企业微信注册审批协调器。
func NewRegistrationApprovalCoordinator(config RegistrationApprovalCoordinatorConfig) (*RegistrationApprovalCoordinator, error) {
	if config.Registrations == nil || config.Gateway == nil || config.KeyDelivery == nil {
		return nil, ErrInvalidRegistrationApprovalDependency
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	adminIDs := append([]string(nil), config.AdminIDs...)
	adminSet := make(map[string]struct{}, len(adminIDs))
	for _, adminID := range adminIDs {
		if strings.TrimSpace(adminID) != adminID || credential.ValidatePrincipalID(adminID) != nil {
			return nil, ErrInvalidRegistrationApprovalDependency
		}
		if _, exists := adminSet[adminID]; exists {
			return nil, ErrInvalidRegistrationApprovalDependency
		}
		adminSet[adminID] = struct{}{}
	}
	return &RegistrationApprovalCoordinator{
		adminIDs:          adminIDs,
		adminSet:          adminSet,
		registrations:     config.Registrations,
		gateway:           config.Gateway,
		keyDelivery:       config.KeyDelivery,
		rejectionDelivery: config.RejectionDelivery,
		logger:            config.Logger,
		snapshots:         make(map[string][]string),
	}, nil
}

// IsAdmin 判断企业微信用户是否属于配置的注册审批管理员。
func (coordinator *RegistrationApprovalCoordinator) IsAdmin(userID string) bool {
	if coordinator == nil {
		return false
	}
	_, exists := coordinator.adminSet[userID]
	return exists
}

// NotifyPending 按配置顺序轮转选择一位管理员并发送新申请通知。
func (coordinator *RegistrationApprovalCoordinator) NotifyPending(ctx context.Context, request machinereg.Request) error {
	if coordinator == nil {
		return ErrInvalidRegistrationApprovalDependency
	}
	coordinator.mu.Lock()
	if len(coordinator.adminIDs) == 0 {
		coordinator.mu.Unlock()
		return nil
	}
	adminID := coordinator.adminIDs[coordinator.nextAdmin]
	coordinator.nextAdmin = (coordinator.nextAdmin + 1) % len(coordinator.adminIDs)
	coordinator.mu.Unlock()

	if err := coordinator.gateway.SendMarkdownTo(ctx, adminID, formatRegistrationApprovalNotification(request)); err != nil {
		args := []any{
			"admin_hash", routerHash(adminID),
			"principal_hash", routerHash(request.PrincipalID),
			"machine_id", safeLogValue(request.MachineID),
			"registration_id", safeLogValue(request.RegistrationID),
		}
		coordinator.logger.Warn("机器注册审批通知失败", append(args, serverErrorLogArgs(err)...)...)
		return err
	}
	coordinator.logger.Debug("机器注册审批通知已发送",
		"admin_hash", routerHash(adminID),
		"principal_hash", routerHash(request.PrincipalID),
		"machine_id", safeLogValue(request.MachineID),
		"registration_id", safeLogValue(request.RegistrationID),
	)
	return nil
}

// PrepareList 生成待审批列表，但不会在管理员完整收到内容前启用编号快照。
func (coordinator *RegistrationApprovalCoordinator) PrepareList(adminID string) (RegistrationApprovalList, error) {
	if !coordinator.IsAdmin(adminID) {
		return RegistrationApprovalList{}, ErrRegistrationApprovalUnauthorized
	}
	requests := coordinator.registrations.ListPending()
	registrationIDs := make([]string, len(requests))
	for index, request := range requests {
		registrationIDs[index] = request.RegistrationID
	}
	return RegistrationApprovalList{
		adminID: adminID, content: formatPendingRegistrationApprovals(requests), registrationIDs: registrationIDs,
	}, nil
}

// CommitList 在列表完整送达后启用该管理员的临时编号快照。
func (coordinator *RegistrationApprovalCoordinator) CommitList(adminID string, list RegistrationApprovalList) error {
	if !coordinator.IsAdmin(adminID) || list.adminID != adminID {
		return ErrRegistrationApprovalUnauthorized
	}
	coordinator.mu.Lock()
	coordinator.snapshots[adminID] = append([]string(nil), list.registrationIDs...)
	coordinator.mu.Unlock()
	return nil
}

// Invalidate 清除管理员最近一次待审批列表快照。
func (coordinator *RegistrationApprovalCoordinator) Invalidate(adminID string) error {
	if !coordinator.IsAdmin(adminID) {
		return ErrRegistrationApprovalUnauthorized
	}
	coordinator.mu.Lock()
	delete(coordinator.snapshots, adminID)
	coordinator.mu.Unlock()
	return nil
}

// Approve 复核管理员快照并依次批准所选稳定申请 ID。
func (coordinator *RegistrationApprovalCoordinator) Approve(ctx context.Context, adminID string, indexes []int) (string, error) {
	selections, err := coordinator.resolveSelection(adminID, indexes)
	if err != nil {
		return "", err
	}
	actor := "wecom:" + adminID
	lines := make([]string, 0, len(selections)+1)
	for _, selection := range selections {
		_, approveErr := coordinator.registrations.Approve(ctx, selection.registrationID, actor, coordinator.keyDelivery)
		if approveErr != nil {
			coordinator.logDecisionFailure("approve", adminID, selection, approveErr)
			lines = append(lines, fmt.Sprintf("%d. %s：批准失败，%s", selection.index, safeRouterLabel(selection.machineID), registrationApprovalFailureText(approveErr)))
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. %s：已批准，Key 已发送给申请人。", selection.index, safeRouterLabel(selection.machineID)))
	}
	return strings.Join(lines, "\n") + "\n\n" + registrationApprovalSnapshotReminder, nil
}

// Reject 复核管理员快照并依次驳回所选稳定申请 ID。
func (coordinator *RegistrationApprovalCoordinator) Reject(ctx context.Context, adminID string, indexes []int) (string, error) {
	selections, err := coordinator.resolveSelection(adminID, indexes)
	if err != nil {
		return "", err
	}
	actor := "wecom:" + adminID
	lines := make([]string, 0, len(selections)+1)
	for _, selection := range selections {
		result, rejectErr := coordinator.registrations.Reject(ctx, selection.registrationID, actor, "", coordinator.rejectionDelivery)
		if rejectErr != nil {
			coordinator.logDecisionFailure("reject", adminID, selection, rejectErr)
			lines = append(lines, fmt.Sprintf("%d. %s：驳回失败，%s", selection.index, safeRouterLabel(selection.machineID), registrationApprovalFailureText(rejectErr)))
			continue
		}
		if result.NotificationSent {
			lines = append(lines, fmt.Sprintf("%d. %s：已驳回，已通知申请人。", selection.index, safeRouterLabel(selection.machineID)))
		} else {
			lines = append(lines, fmt.Sprintf("%d. %s：已驳回，但申请人通知发送失败。", selection.index, safeRouterLabel(selection.machineID)))
		}
	}
	return strings.Join(lines, "\n") + "\n\n" + registrationApprovalSnapshotReminder, nil
}

func (coordinator *RegistrationApprovalCoordinator) resolveSelection(adminID string, indexes []int) ([]registrationApprovalSelection, error) {
	if !coordinator.IsAdmin(adminID) {
		return nil, ErrRegistrationApprovalUnauthorized
	}
	coordinator.mu.Lock()
	snapshot, exists := coordinator.snapshots[adminID]
	delete(coordinator.snapshots, adminID)
	coordinator.mu.Unlock()
	if !exists {
		return nil, ErrRegistrationApprovalSnapshotMissing
	}
	if !validRegistrationApprovalIndexes(indexes) {
		return nil, ErrRegistrationApprovalInvalidIndexes
	}
	for _, index := range indexes {
		if index > len(snapshot) {
			return nil, ErrRegistrationApprovalSnapshotChanged
		}
	}
	current := coordinator.registrations.ListPending()
	selections := make([]registrationApprovalSelection, 0, len(indexes))
	for _, index := range indexes {
		position := index - 1
		if position >= len(current) || current[position].RegistrationID != snapshot[position] {
			return nil, ErrRegistrationApprovalSnapshotChanged
		}
		selections = append(selections, registrationApprovalSelection{
			index:          index,
			registrationID: snapshot[position],
			machineID:      current[position].MachineID,
		})
	}
	return selections, nil
}

func (coordinator *RegistrationApprovalCoordinator) logDecisionFailure(action, adminID string, selection registrationApprovalSelection, err error) {
	args := []any{
		"action", action,
		"admin_hash", routerHash(adminID),
		"machine_id", safeLogValue(selection.machineID),
		"registration_id", safeLogValue(selection.registrationID),
	}
	coordinator.logger.Warn("企业微信机器注册审批失败", append(args, serverErrorLogArgs(err)...)...)
}

func validRegistrationApprovalIndexes(indexes []int) bool {
	if len(indexes) == 0 || len(indexes) > maxRegistrationApprovalBatch {
		return false
	}
	seen := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		if index < 1 {
			return false
		}
		if _, exists := seen[index]; exists {
			return false
		}
		seen[index] = struct{}{}
	}
	return true
}

func formatRegistrationApprovalNotification(request machinereg.Request) string {
	return fmt.Sprintf(
		"有新的机器注册申请：\n用户：%s\n机器：%s\n来源：%s\n请执行 /ls-reg 查看当前待审批列表。",
		safeRouterLabel(request.PrincipalID),
		safeRouterLabel(request.MachineID),
		registrationApprovalSources(request.AllowedSources),
	)
}

func formatPendingRegistrationApprovals(requests []machinereg.Request) string {
	if len(requests) == 0 {
		return "当前没有待审批机器注册申请。"
	}
	lines := []string{"待审批机器注册："}
	for index, request := range requests {
		lines = append(lines, fmt.Sprintf(
			"%d. 用户：%s；机器：%s；来源：%s；申请时间：%s",
			index+1,
			safeRouterLabel(request.PrincipalID),
			safeRouterLabel(request.MachineID),
			registrationApprovalSources(request.AllowedSources),
			request.RequestedAt.UTC().Format(time.RFC3339),
		))
	}
	return strings.Join(lines, "\n")
}

func registrationApprovalSources(rules []credential.SourceRule) string {
	if len(rules) == 0 {
		return "未配置"
	}
	values := make([]string, len(rules))
	for index, rule := range rules {
		values[index] = safeRouterLabel(string(rule))
	}
	return strings.Join(values, ",")
}

func registrationApprovalFailureText(err error) string {
	switch {
	case errors.Is(err, machinereg.ErrRequestNotFound):
		return "申请已被处理或不存在。"
	case errors.Is(err, machinereg.ErrMachineExists):
		return "该用户机器凭据已经存在。"
	case errors.Is(err, machinereg.ErrDeliveryFailed):
		return "Key 交付失败，申请仍保留。"
	case errors.Is(err, machinereg.ErrRollbackFailed):
		return "Key 交付失败且凭据回滚失败，请检查服务端日志。"
	case errors.Is(err, machinereg.ErrCleanupFailed):
		return "Key 已交付但申请清理失败，请检查服务端日志。"
	case errors.Is(err, credential.ErrCredentialConflict):
		return "凭据状态冲突，申请仍保留。"
	default:
		return "操作失败，请检查服务端日志。"
	}
}
