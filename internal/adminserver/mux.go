package adminserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

var errInvalidMethodMux = errors.New("HPAP 方法路由配置无效")

// MethodHandler 声明处理器负责的方法集合。
type MethodHandler interface {
	Handler
	Methods() []adminproto.Method
}

// MethodMux 把固定 HPAP 方法分派给单一职责处理器。
type MethodMux struct {
	handlers map[adminproto.Method]Handler
}

// NewMethodMux 创建拒绝重复或未知方法注册的固定路由表。
func NewMethodMux(handlers ...MethodHandler) (*MethodMux, error) {
	mux := &MethodMux{handlers: make(map[adminproto.Method]Handler)}
	for _, handler := range handlers {
		if handler == nil {
			return nil, errInvalidMethodMux
		}
		for _, method := range handler.Methods() {
			if !adminproto.IsKnownMethod(method) {
				return nil, fmt.Errorf("%w: 未知方法 %s", errInvalidMethodMux, method)
			}
			if _, exists := mux.handlers[method]; exists {
				return nil, fmt.Errorf("%w: 方法重复 %s", errInvalidMethodMux, method)
			}
			mux.handlers[method] = handler
		}
	}
	return mux, nil
}

// Handle 把请求分派给唯一注册的处理器。
func (mux *MethodMux) Handle(ctx context.Context, request adminproto.Request) (HandleResult, error) {
	if mux == nil {
		return HandleResult{}, errInvalidMethodMux
	}
	handler := mux.handlers[request.Method]
	if handler == nil {
		return HandleResult{}, fmt.Errorf("%w: 方法未注册 %s", errInvalidMethodMux, request.Method)
	}
	return handler.Handle(ctx, request)
}
