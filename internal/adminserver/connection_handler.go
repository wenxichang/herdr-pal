package adminserver

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/server"
)

var errInvalidConnectionHandler = errors.New("HPAP Connection Handler 依赖无效")

// ConnectionHandler 处理 HPRP 活动连接的只读查询和显式断开。
type ConnectionHandler struct {
	connections ConnectionManager
	now         func() time.Time
}

// NewConnectionHandler 创建 HPRP 连接管理处理器。
func NewConnectionHandler(connections ConnectionManager, now func() time.Time) (*ConnectionHandler, error) {
	if connections == nil {
		return nil, errInvalidConnectionHandler
	}
	if now == nil {
		now = time.Now
	}
	return &ConnectionHandler{connections: connections, now: now}, nil
}

// Methods 返回当前处理器负责的 Connection 方法。
func (handler *ConnectionHandler) Methods() []adminproto.Method {
	return []adminproto.Method{
		adminproto.MethodConnectionList,
		adminproto.MethodConnectionShow,
		adminproto.MethodConnectionDisconnect,
	}
}

// Handle 执行连接查询或只撤下当前连接的管理动作。
func (handler *ConnectionHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil {
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
	views := handler.connections.Connections()
	sort.Slice(views, func(left, right int) bool { return connectionSortKey(views[left]) < connectionSortKey(views[right]) })
	items := make([]adminproto.Connection, 0, limit)
	lastKey := ""
	more := false
	for _, view := range views {
		key := connectionSortKey(view)
		if anchor != "" && key <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, connectionView(view))
		lastKey = key
	}
	result := adminproto.ConnectionListResult{ObservedAt: handler.now().UTC(), Items: items}
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
	view, exists := handler.connections.Connection(params.ConnectionID)
	if !exists {
		return keyError(request.ID, adminproto.CodeConnectionNotFound, "连接不存在"), nil
	}
	return keySuccess(request.ID, adminproto.ConnectionResult{ObservedAt: handler.now().UTC(), Connection: connectionView(view)}, connectionAuditTarget(params.ConnectionID))
}

func (handler *ConnectionHandler) disconnect(request adminproto.Request) (HandleResult, error) {
	params, result := decodeConnectionID(request)
	if result.Response.Error != nil {
		return result, nil
	}
	if !handler.connections.DisconnectConnection(params.ConnectionID, "admin request") {
		return keyError(request.ID, adminproto.CodeConnectionNotFound, "连接不存在"), nil
	}
	return keySuccess(request.ID, adminproto.ConnectionDisconnectResult{
		ObservedAt: handler.now().UTC(), ConnectionID: params.ConnectionID, Disconnected: true,
	}, connectionAuditTarget(params.ConnectionID))
}

func decodeConnectionID(request adminproto.Request) (adminproto.ConnectionIDParams, HandleResult) {
	var params adminproto.ConnectionIDParams
	if err := adminproto.DecodeParams(request.Params, &params); err != nil || strings.TrimSpace(params.ConnectionID) == "" || len(params.ConnectionID) > 256 {
		return adminproto.ConnectionIDParams{}, keyError(request.ID, adminproto.CodeArgumentInvalid, "connection_id 无效")
	}
	return params, HandleResult{}
}

func connectionView(view server.ConnectionView) adminproto.Connection {
	return adminproto.Connection{
		ConnectionID: view.ConnectionID, CredentialID: view.CredentialID, PrincipalID: view.PrincipalID, MachineID: view.MachineID,
		Implementation: adminproto.Implementation{
			Name: view.Implementation.Name, Version: view.Implementation.Version,
			OS: view.Implementation.OS, Arch: view.Implementation.Arch,
		},
		SourceIP: view.SourceIP, ConnectedAt: view.ConnectedAt.UTC(), LastHeartbeatAt: view.LastHeartbeatAt.UTC(),
		LastSnapshotAt: view.LastSnapshotAt.UTC(), SnapshotSequence: view.SnapshotSequence,
		SessionCount: view.SessionCount, Capabilities: append([]string(nil), view.Capabilities...), Ready: view.Ready,
	}
}

func connectionSortKey(view server.ConnectionView) string {
	return view.PrincipalID + "\x00" + view.MachineID + "\x00" + view.ConnectionID
}

func connectionAuditTarget(connectionID string) string {
	return "connection_id=" + connectionID
}
