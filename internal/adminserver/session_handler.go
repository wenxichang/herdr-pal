package adminserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

var errInvalidSessionHandler = errors.New("HPAP Session Handler 依赖无效")

// SessionHandler 把 HPAP 会话查询适配到共享管理服务。
type SessionHandler struct {
	service *adminservice.Service
}

// NewSessionHandler 创建不具备终端读取能力的会话查询处理器。
func NewSessionHandler(service *adminservice.Service) (*SessionHandler, error) {
	if service == nil {
		return nil, errInvalidSessionHandler
	}
	return &SessionHandler{service: service}, nil
}

// Methods 返回当前处理器负责的 Session 方法。
func (handler *SessionHandler) Methods() []adminproto.Method {
	return []adminproto.Method{adminproto.MethodSessionList}
}

// Handle 返回不含终端内容且不修改用户路由状态的会话列表。
func (handler *SessionHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil || handler.service == nil || request.Method != adminproto.MethodSessionList {
		return HandleResult{}, errInvalidSessionHandler
	}
	var params adminproto.SessionListParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || !validOptionalLabel(params.PrincipalID) || params.MachineID != "" && hprp.ValidateMachineID(params.MachineID) != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "会话列表参数无效"), nil
	}
	limit, err := normalizePageLimit(params.Limit)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "limit 必须在 1 到 500 之间"), nil
	}
	anchor, err := decodePageToken(params.PageToken, request.Method)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "page_token 无效"), nil
	}
	sessions := handler.service.ListSessions(adminservice.SessionFilter{PrincipalID: params.PrincipalID, MachineID: params.MachineID})
	items := make([]adminproto.Session, 0, limit)
	lastKey := ""
	more := false
	for _, session := range sessions {
		key := sessionSortKey(session)
		if anchor != "" && key <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, session)
		lastKey = key
	}
	result := adminproto.SessionListResult{ObservedAt: handler.service.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodePageToken(request.Method, lastKey)
		if err != nil {
			return HandleResult{}, err
		}
	}
	return keySuccess(request.ID, result, sessionFilterAuditTarget(params))
}

func sessionSortKey(view adminservice.Session) string {
	return fmt.Sprintf("%s\x00%020d\x00%s\x00%s\x00%s", view.PrincipalID, view.Number, view.Target.MachineID, view.Target.SlotID, view.Target.SessionID)
}

func validOptionalLabel(value string) bool {
	if value == "" {
		return true
	}
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || len(value) > hprp.MaxLabelBytes {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func sessionFilterAuditTarget(params adminproto.SessionListParams) string {
	parts := make([]string, 0, 2)
	if params.PrincipalID != "" {
		parts = append(parts, "principal_hash="+auditValueHash(params.PrincipalID))
	}
	if params.MachineID != "" {
		parts = append(parts, "machine_id="+params.MachineID)
	}
	return strings.Join(parts, " ")
}
