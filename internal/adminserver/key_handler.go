package adminserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

var errInvalidKeyHandler = errors.New("HPAP Key Handler 依赖无效")

// KeyHandler 把 HPAP Key 请求适配到共享管理服务。
type KeyHandler struct {
	service *adminservice.Service
	logger  *slog.Logger
}

// NewKeyHandler 创建只负责 HPAP 参数、分页和响应编码的 Key 处理器。
func NewKeyHandler(service *adminservice.Service, logger *slog.Logger) (*KeyHandler, error) {
	if service == nil || logger == nil {
		return nil, errInvalidKeyHandler
	}
	return &KeyHandler{service: service, logger: logger}, nil
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

// Handle 解码 HPAP 请求并委托共享管理服务执行实际业务规则。
func (handler *KeyHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil || handler.service == nil {
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
		return handler.setEnabled(request, true)
	case adminproto.MethodKeyDisable:
		return handler.setEnabled(request, false)
	case adminproto.MethodKeyDelete:
		return handler.delete(request)
	case adminproto.MethodKeySourceList:
		return handler.sourceList(request)
	case adminproto.MethodKeySourceAdd:
		return handler.sourceMutate(request, adminservice.SourceAdd)
	case adminproto.MethodKeySourceRemove:
		return handler.sourceMutate(request, adminservice.SourceRemove)
	case adminproto.MethodKeySourceSet:
		return handler.sourceMutate(request, adminservice.SourceSet)
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
	result, err := handler.service.IssueCredential(adminservice.IssueCredentialInput{
		PrincipalID: params.PrincipalID,
		MachineID:   params.MachineID,
		Sources:     params.Sources,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, result, credentialAuditTarget(result.Credential.CredentialID))
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
	credentials := handler.service.ListCredentials()
	items := make([]adminproto.Credential, 0, limit)
	var lastID uint64
	more := false
	for _, current := range credentials {
		if current.CredentialID <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, current)
		lastID = current.CredentialID
	}
	result := adminproto.KeyListResult{ObservedAt: handler.service.ObservedAt(), Items: items}
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
	credentialView, err := handler.service.ShowCredential(params.CredentialID)
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, adminproto.CredentialResult{Credential: credentialView}, credentialAuditTarget(credentialView.CredentialID))
}

func (handler *KeyHandler) setEnabled(request adminproto.Request, enabled bool) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	mutation, err := handler.service.SetCredentialEnabled(params.CredentialID, enabled)
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, mutation, credentialAuditTarget(mutation.Credential.CredentialID))
}

func (handler *KeyHandler) delete(request adminproto.Request) (HandleResult, error) {
	var params adminproto.KeyDeleteParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 || !params.Confirm {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "删除 Key 必须指定 credential_id 并确认"), nil
	}
	result, err := handler.service.DeleteCredential(params.CredentialID)
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, result, credentialAuditTarget(result.CredentialID))
}

func (handler *KeyHandler) sourceList(request adminproto.Request) (HandleResult, error) {
	params, result := decodeCredentialID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	sources, err := handler.service.ListSources(params.CredentialID)
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, adminproto.KeySourceListResult{CredentialID: params.CredentialID, Sources: sources}, credentialAuditTarget(params.CredentialID))
}

func (handler *KeyHandler) sourceMutate(request adminproto.Request, operation adminservice.SourceOperation) (HandleResult, error) {
	var params adminproto.KeySourceMutationParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Key 来源参数无效"), nil
	}
	if len(params.Sources) == 0 {
		return keyError(request.ID, adminproto.CodeCredentialSourceRequired, "Key 至少需要一个来源规则"), nil
	}
	result, err := handler.service.MutateSources(params.CredentialID, operation, params.Sources)
	if err != nil {
		return handler.serviceFailure(request, err)
	}
	return keySuccess(request.ID, result, credentialAuditTarget(result.Credential.CredentialID))
}

func decodeCredentialID(request adminproto.Request) (adminproto.CredentialIDParams, HandleResult) {
	var params adminproto.CredentialIDParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || params.CredentialID == 0 {
		return adminproto.CredentialIDParams{}, keyError(request.ID, adminproto.CodeArgumentInvalid, "credential_id 无效")
	}
	return params, HandleResult{}
}

func (handler *KeyHandler) serviceFailure(request adminproto.Request, err error) (HandleResult, error) {
	code, message := hpapServiceError(err)
	if adminservice.ErrorCodeOf(err) == adminservice.CodeServerBusy {
		message = "Key ID 已耗尽"
	}
	if code == adminproto.CodeServerInternal {
		return handler.internalFailure(request, err)
	}
	return keyError(request.ID, code, message), nil
}

func (handler *KeyHandler) internalFailure(request adminproto.Request, err error) (HandleResult, error) {
	if handler.logger != nil {
		handler.logger.Error("HPAP Key 管理失败", "method", request.Method, "request_hash", auditRequestHash(request.ID), "error_type", fmt.Sprintf("%T", err), "reason", "AdminService 操作失败")
	}
	return keyError(request.ID, adminproto.CodeServerInternal, "Key 管理失败"), nil
}

func hpapServiceError(err error) (adminproto.ErrorCode, string) {
	switch adminservice.ErrorCodeOf(err) {
	case adminservice.CodeInvalidArgument:
		return adminproto.CodeArgumentInvalid, "Key 参数无效"
	case adminservice.CodeCredentialNotFound:
		return adminproto.CodeCredentialNotFound, "Key 不存在"
	case adminservice.CodeCredentialConflict:
		return adminproto.CodeCredentialConflict, "Key 冲突"
	case adminservice.CodeSourceRequired:
		return adminproto.CodeCredentialSourceRequired, "Key 至少需要一个来源规则"
	case adminservice.CodeSourceInvalid:
		return adminproto.CodeCredentialSourceInvalid, "Key 来源规则无效"
	case adminservice.CodeConnectionNotFound:
		return adminproto.CodeConnectionNotFound, "连接不存在"
	case adminservice.CodeServerBusy:
		return adminproto.CodeServerBusy, "服务端正忙"
	default:
		return adminproto.CodeServerInternal, "管理操作失败"
	}
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

func credentialAuditTarget(credentialID uint64) string {
	return "credential_id=" + strconv.FormatUint(credentialID, 10)
}
