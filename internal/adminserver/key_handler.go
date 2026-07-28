package adminserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

var errInvalidKeyHandler = errors.New("HPAP Key Handler 依赖无效")

// KeyHandler 处理 Key 生命周期、来源策略和列表查询方法。
type KeyHandler struct {
	credentials CredentialManager
	connections ConnectionManager
	logger      *slog.Logger
	now         func() time.Time
}

// NewKeyHandler 创建可动态修改当前 CredentialStore 的 Key 管理处理器。
func NewKeyHandler(credentials CredentialManager, connections ConnectionManager, logger *slog.Logger, now func() time.Time) (*KeyHandler, error) {
	if credentials == nil || connections == nil || logger == nil {
		return nil, errInvalidKeyHandler
	}
	if now == nil {
		now = time.Now
	}
	return &KeyHandler{credentials: credentials, connections: connections, logger: logger, now: now}, nil
}

// Methods 返回当前处理器负责的固定 HPAP 方法集合。
func (handler *KeyHandler) Methods() []adminproto.Method {
	return []adminproto.Method{
		adminproto.MethodKeyIssue,
		adminproto.MethodKeyList,
		adminproto.MethodKeyShow,
		adminproto.MethodKeyEnable,
		adminproto.MethodKeyDisable,
		adminproto.MethodKeyDelete,
		adminproto.MethodKeySourceList,
		adminproto.MethodKeySourceAdd,
		adminproto.MethodKeySourceRemove,
		adminproto.MethodKeySourceSet,
	}
}

// Handle 校验具体参数，并保证持久化成功后才执行连接撤下。
func (handler *KeyHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil {
		return HandleResult{}, errInvalidKeyHandler
	}
	switch request.Method {
	case adminproto.MethodKeyIssue:
		return handler.issue(request)
	case adminproto.MethodKeyList:
		return handler.list(request)
	case adminproto.MethodKeyShow:
		return handler.show(request)
	case adminproto.MethodKeyEnable:
		return handler.enable(request)
	case adminproto.MethodKeyDisable:
		return handler.disable(request)
	case adminproto.MethodKeyDelete:
		return handler.delete(request)
	case adminproto.MethodKeySourceList:
		return handler.sourceList(request)
	case adminproto.MethodKeySourceAdd:
		return handler.sourceMutate(request, "add")
	case adminproto.MethodKeySourceRemove:
		return handler.sourceMutate(request, "remove")
	case adminproto.MethodKeySourceSet:
		return handler.sourceMutate(request, "set")
	default:
		return HandleResult{}, fmt.Errorf("%w: %s", errInvalidKeyHandler, request.Method)
	}
}

func (handler *KeyHandler) issue(request adminproto.Request) (HandleResult, error) {
	var params adminproto.KeyIssueParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Key 签发参数无效"), nil
	}
	var expiresAt *time.Time
	if params.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *params.ExpiresAt)
		if err != nil {
			return keyError(request.ID, adminproto.CodeArgumentInvalid, "expires_at 必须是 RFC3339 时间"), nil
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	token, record, err := handler.credentials.Issue(params.PrincipalID, params.MachineID, params.Sources, expiresAt)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	result := adminproto.KeyIssueResult{Token: token, Credential: credentialView(record)}
	return keySuccess(request.ID, result, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) list(request adminproto.Request) (HandleResult, error) {
	var params adminproto.KeyListParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Key 列表参数无效"), nil
	}
	limit, err := normalizePageLimit(params.Limit)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "limit 必须在 1 到 500 之间"), nil
	}
	anchor, err := decodeCredentialPageToken(params.PageToken, request.Method)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "page_token 无效"), nil
	}
	records := handler.credentials.List()
	items := make([]adminproto.Credential, 0, limit)
	var lastID uint64
	more := false
	for _, record := range records {
		if record.CredentialID <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, credentialView(record))
		lastID = record.CredentialID
	}
	result := adminproto.KeyListResult{ObservedAt: handler.now().UTC(), Items: items}
	if more {
		result.NextPageToken, err = encodeCredentialPageToken(request.Method, lastID)
		if err != nil {
			return handler.internalFailure(request, err)
		}
	}
	return keySuccess(request.ID, result, "")
}

func (handler *KeyHandler) show(request adminproto.Request) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	record, err := handler.credentials.Show(params.CredentialID)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	return keySuccess(request.ID, adminproto.CredentialResult{Credential: credentialView(record)}, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) enable(request adminproto.Request) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	record, err := handler.credentials.Enable(params.CredentialID)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	return keySuccess(request.ID, adminproto.CredentialMutationResult{Credential: credentialView(record)}, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) disable(request adminproto.Request) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	record, err := handler.credentials.Disable(params.CredentialID)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	disconnected := handler.connections.DisconnectCredential(record.CredentialID, "credential disabled")
	return keySuccess(request.ID, adminproto.CredentialMutationResult{
		Credential: credentialView(record), DisconnectedConnections: disconnected,
	}, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) delete(request adminproto.Request) (HandleResult, error) {
	var params adminproto.KeyDeleteParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 || !params.Confirm {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "删除 Key 必须指定 credential_id 并确认"), nil
	}
	record, err := handler.credentials.Delete(params.CredentialID)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	disconnected := handler.connections.DisconnectCredential(record.CredentialID, "credential deleted")
	return keySuccess(request.ID, adminproto.KeyDeleteResult{
		CredentialID: record.CredentialID, Deleted: true, DisconnectedConnections: disconnected,
	}, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) sourceList(request adminproto.Request) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	record, err := handler.credentials.Show(params.CredentialID)
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	return keySuccess(request.ID, adminproto.KeySourceListResult{
		CredentialID: record.CredentialID, Sources: sourceStrings(record.AllowedSources),
	}, credentialAuditTarget(record.CredentialID))
}

func (handler *KeyHandler) sourceMutate(request adminproto.Request, operation string) (HandleResult, error) {
	var params adminproto.KeySourceMutationParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Key 来源参数无效"), nil
	}
	if len(params.Sources) == 0 {
		return keyError(request.ID, adminproto.CodeCredentialSourceRequired, "Key 至少需要一个来源规则"), nil
	}
	var (
		record credential.Record
		err    error
	)
	switch operation {
	case "add":
		record, err = handler.credentials.AddSources(params.CredentialID, params.Sources)
	case "remove":
		record, err = handler.credentials.RemoveSources(params.CredentialID, params.Sources)
	case "set":
		record, err = handler.credentials.SetSources(params.CredentialID, params.Sources)
	default:
		return HandleResult{}, errInvalidKeyHandler
	}
	if err != nil {
		return handler.credentialFailure(request, err)
	}
	disconnected := 0
	if operation != "add" {
		disconnected = handler.connections.RevalidateCredentialSource(record.CredentialID, record.AllowedSources, "credential source policy changed")
	}
	return keySuccess(request.ID, adminproto.CredentialMutationResult{
		Credential: credentialView(record), DisconnectedConnections: disconnected,
	}, credentialAuditTarget(record.CredentialID))
}

func decodeCredentialID(request adminproto.Request) (adminproto.CredentialIDParams, HandleResult) {
	var params adminproto.CredentialIDParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 {
		return adminproto.CredentialIDParams{}, keyError(request.ID, adminproto.CodeArgumentInvalid, "credential_id 无效")
	}
	return params, HandleResult{}
}

func (handler *KeyHandler) credentialFailure(request adminproto.Request, err error) (HandleResult, error) {
	switch {
	case errors.Is(err, credential.ErrCredentialNotFound):
		return keyError(request.ID, adminproto.CodeCredentialNotFound, "Key 不存在"), nil
	case errors.Is(err, credential.ErrCredentialConflict):
		return keyError(request.ID, adminproto.CodeCredentialConflict, "Key 冲突"), nil
	case errors.Is(err, credential.ErrSourceRequired):
		return keyError(request.ID, adminproto.CodeCredentialSourceRequired, "Key 至少需要一个来源规则"), nil
	case errors.Is(err, credential.ErrSourceInvalid):
		return keyError(request.ID, adminproto.CodeCredentialSourceInvalid, "Key 来源规则无效"), nil
	case errors.Is(err, credential.ErrInvalidRecord):
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Key 参数无效"), nil
	case errors.Is(err, credential.ErrCredentialIDExhausted):
		return keyError(request.ID, adminproto.CodeServerBusy, "Key ID 已耗尽"), nil
	default:
		return handler.internalFailure(request, err)
	}
}

func (handler *KeyHandler) internalFailure(request adminproto.Request, err error) (HandleResult, error) {
	if handler.logger != nil {
		handler.logger.Error("HPAP Key 管理失败", "method", request.Method, "request_hash", auditRequestHash(request.ID), "error_type", fmt.Sprintf("%T", err), "reason", "CredentialStore 操作失败")
	}
	return keyError(request.ID, adminproto.CodeServerInternal, "Key 管理失败"), nil
}

func keySuccess(requestID string, value any, target string) (HandleResult, error) {
	response, err := adminproto.NewResultResponse(requestID, value)
	if err != nil {
		return HandleResult{}, err
	}
	return HandleResult{Response: response, AuditTarget: target}, nil
}

func keyError(requestID string, code adminproto.ErrorCode, message string) HandleResult {
	response, err := adminproto.NewErrorResponse(requestID, adminproto.Error{Code: code, Message: message})
	if err != nil {
		return HandleResult{}
	}
	return HandleResult{Response: response}
}

func credentialView(record credential.Record) adminproto.Credential {
	var expiresAt *time.Time
	if record.ExpiresAt != nil {
		value := record.ExpiresAt.UTC()
		expiresAt = &value
	}
	return adminproto.Credential{
		CredentialID: record.CredentialID, PrincipalID: record.PrincipalID, MachineID: record.MachineID,
		Status: string(record.Status), AllowedSources: sourceStrings(record.AllowedSources), ExpiresAt: expiresAt,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func sourceStrings(rules []credential.SourceRule) []string {
	values := make([]string, len(rules))
	for index, rule := range rules {
		values[index] = string(rule)
	}
	return values
}

func credentialAuditTarget(credentialID uint64) string {
	return "credential_id=" + strconv.FormatUint(credentialID, 10)
}
