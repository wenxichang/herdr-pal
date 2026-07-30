package adminserver

import (
	"context"
	"errors"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

var errInvalidConnectionHandler = errors.New("HPAP Connection Handler 依赖无效")

// ConnectionHandler 把 HPAP 连接管理请求适配到共享管理服务。
type ConnectionHandler struct {
	service *adminservice.Service
}

// NewConnectionHandler 创建只负责 HPAP 参数、分页和响应编码的连接处理器。
func NewConnectionHandler(service *adminservice.Service) (*ConnectionHandler, error) {
	if service == nil {
		return nil, errInvalidConnectionHandler
	}
	return &ConnectionHandler{service: service}, nil
}

// Methods 返回当前处理器负责的 Connection 方法。
func (handler *ConnectionHandler) Methods() []adminproto.Method {
	return []adminproto.Method{
		adminproto.MethodConnectionList,
		adminproto.MethodConnectionShow,
		adminproto.MethodConnectionDisconnect,
	}
}

// Handle 解码 HPAP 请求并委托共享管理服务执行连接操作。
func (handler *ConnectionHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil || handler.service == nil {
		return HandleResult{}, errInvalidConnectionHandler
	}
	switch request.Method {
	case adminproto.MethodConnectionList:
		return handler.list(request)
	case adminproto.MethodConnectionShow:
		return handler.show(request)
	case adminproto.MethodConnectionDisconnect:
		return handler.disconnect(request)
	default:
		return HandleResult{}, errInvalidConnectionHandler
	}
}

func (handler *ConnectionHandler) list(request adminproto.Request) (HandleResult, error) {
	var params adminproto.ConnectionListParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "连接列表参数无效"), nil
	}
	limit, err := normalizePageLimit(params.Limit)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "limit 必须在 1 到 500 之间"), nil
	}
	anchor, err := decodePageToken(params.PageToken, request.Method)
	if err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "page_token 无效"), nil
	}
	connections := handler.service.ListConnections()
	items := make([]adminproto.Connection, 0, limit)
	lastKey := ""
	more := false
	for _, connection := range connections {
		key := connectionSortKey(connection)
		if anchor != "" && key <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, connection)
		lastKey = key
	}
	result := adminproto.ConnectionListResult{ObservedAt: handler.service.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodePageToken(request.Method, lastKey)
		if err != nil {
			return HandleResult{}, err
		}
	}
	return keySuccess(request.ID, result, "")
}

func (handler *ConnectionHandler) show(request adminproto.Request) (HandleResult, error) {
	params, result := decodeConnectionID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	connection, err := handler.service.ShowConnection(params.ConnectionID)
	if err != nil {
		code, message := hpapServiceError(err)
		return keyError(request.ID, code, message), nil
	}
	return keySuccess(request.ID, adminproto.ConnectionResult{ObservedAt: handler.service.ObservedAt(), Connection: connection}, connectionAuditTarget(params.ConnectionID))
}

func (handler *ConnectionHandler) disconnect(request adminproto.Request) (HandleResult, error) {
	params, result := decodeConnectionID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	if err := handler.service.DisconnectConnection(params.ConnectionID); err != nil {
		code, message := hpapServiceError(err)
		return keyError(request.ID, code, message), nil
	}
	return keySuccess(request.ID, adminproto.ConnectionDisconnectResult{
		ObservedAt: handler.service.ObservedAt(), ConnectionID: params.ConnectionID, Disconnected: true,
	}, connectionAuditTarget(params.ConnectionID))
}

func decodeConnectionID(request adminproto.Request) (adminproto.ConnectionIDParams, HandleResult) {
	var params adminproto.ConnectionIDParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || strings.TrimSpace(params.ConnectionID) == "" || len(params.ConnectionID) > 256 {
		return adminproto.ConnectionIDParams{}, keyError(request.ID, adminproto.CodeArgumentInvalid, "connection_id 无效")
	}
	return params, HandleResult{}
}

func connectionSortKey(view adminservice.Connection) string {
	return view.PrincipalID + "\x00" + view.MachineID + "\x00" + view.ConnectionID
}

func connectionAuditTarget(connectionID string) string {
	return "connection_id=" + connectionID
}
