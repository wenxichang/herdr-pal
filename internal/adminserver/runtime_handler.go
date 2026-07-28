package adminserver

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

var errInvalidRuntimeHandler = errors.New("HPAP Runtime Handler 依赖无效")

// RuntimeHandler 处理服务状态、动态 debug 和响应后停止请求。
type RuntimeHandler struct {
	runtime  RuntimeInspector
	stopping atomic.Bool
}

// NewRuntimeHandler 创建服务运行管理处理器。
func NewRuntimeHandler(runtime RuntimeInspector) (*RuntimeHandler, error) {
	if runtime == nil {
		return nil, errInvalidRuntimeHandler
	}
	return &RuntimeHandler{runtime: runtime}, nil
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
	if handler == nil {
		return HandleResult{}, errInvalidRuntimeHandler
	}
	if err := decodeEmptyParams(request); err != nil {
		return keyError(request.ID, adminproto.CodeArgumentInvalid, "Server 方法不接受额外参数"), nil
	}
	switch request.Method {
	case adminproto.MethodServerStatus:
		return keySuccess(request.ID, handler.runtime.Status(), "server")
	case adminproto.MethodServerDebugEnable:
		handler.runtime.EnableDebug()
		status := handler.runtime.Status()
		return keySuccess(request.ID, adminproto.ServerDebugResult{DebugEnabled: status.DebugEnabled, BaseLogLevel: status.BaseLogLevel}, "server")
	case adminproto.MethodServerDebugDisable:
		handler.runtime.DisableDebug()
		status := handler.runtime.Status()
		return keySuccess(request.ID, adminproto.ServerDebugResult{DebugEnabled: status.DebugEnabled, BaseLogLevel: status.BaseLogLevel}, "server")
	case adminproto.MethodServerStop:
		if !handler.stopping.CompareAndSwap(false, true) {
			return keyError(request.ID, adminproto.CodeServerBusy, "服务端已经开始停止"), nil
		}
		result, err := keySuccess(request.ID, adminproto.ServerStopResult{Stopping: true}, "server")
		if err != nil {
			handler.stopping.Store(false)
			return HandleResult{}, err
		}
		result.AfterWrite = func() { handler.runtime.RequestStop() }
		return result, nil
	default:
		return HandleResult{}, errInvalidRuntimeHandler
	}
}

func decodeEmptyParams(request adminproto.Request) error {
	var params adminproto.EmptyParams
	return adminproto.DecodeParams(request.Params, &params)
}
