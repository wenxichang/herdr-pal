package adminserver

import (
	"context"
	"errors"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

var errInvalidRuntimeHandler = errors.New("HPAP Runtime Handler 依赖无效")

// RuntimeHandler 把 HPAP 运行控制请求适配到共享管理服务。
type RuntimeHandler struct {
	service *adminservice.Service
}

// NewRuntimeHandler 创建服务运行管理处理器。
func NewRuntimeHandler(service *adminservice.Service) (*RuntimeHandler, error) {
	if service == nil {
		return nil, errInvalidRuntimeHandler
	}
	return &RuntimeHandler{service: service}, nil
}

// Methods 返回当前处理器负责的 Server 方法。
func (handler *RuntimeHandler) Methods() []adminproto.Method {
	return []adminproto.Method{
		adminproto.MethodServerStatus,
		adminproto.MethodServerStop,
		adminproto.MethodServerDebugEnable,
		adminproto.MethodServerDebugDisable,
	}
}

// Handle 执行不持久化配置的 Server 管理动作。
func (handler *RuntimeHandler) Handle(_ context.Context, request adminproto.Request) (HandleResult, error) {
	if handler == nil || handler.service == nil {
		return HandleResult{}, errInvalidRuntimeHandler
	}
	if err := decodeEmptyParams(request); err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Server 方法不接受额外参数"), nil
	}
	switch request.Method {
	case adminproto.MethodServerStatus:
		return keySuccess(request.ID, handler.service.Status(), "server")
	case adminproto.MethodServerDebugEnable:
		return keySuccess(request.ID, handler.service.SetDebug(true), "server")
	case adminproto.MethodServerDebugDisable:
		return keySuccess(request.ID, handler.service.SetDebug(false), "server")
	case adminproto.MethodServerStop:
		action, err := handler.service.PrepareStop()
		if err != nil {
			code, message := hpapServiceError(err)
			if code == adminproto.CodeServerBusy {
				message = "服务端已经开始停止"
			}
			return keyError(request.ID, code, message), nil
		}
		result, err := keySuccess(request.ID, adminproto.ServerStopResult{Stopping: true}, "server")
		if err != nil {
			action.Rollback()
			return HandleResult{}, err
		}
		result.AfterWrite = action.Commit
		result.AfterWriteFailure = action.Rollback
		return result, nil
	default:
		return HandleResult{}, errInvalidRuntimeHandler
	}
}

func decodeEmptyParams(request adminproto.Request) error {
	var params adminproto.EmptyParams
	return adminproto.DecodeParams(request.Params, &params)
}
