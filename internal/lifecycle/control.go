package lifecycle

import (
	"context"
	"errors"
)

const controlMethodStatus = "status"

var (
	// ErrUnsupported 表示当前平台不支持 Pal 自动启动与自守护能力。
	ErrUnsupported = errors.New("当前平台不支持 Pal 自动启动与自守护")
	// ErrControlProtocol 表示本地健康端点收到了无效请求或响应。
	ErrControlProtocol = errors.New("Pal 本地健康协议无效")
	// ErrControlUnavailable 表示本地健康端点当前不可连接。
	ErrControlUnavailable = errors.New("Pal 本地健康端点不可用")
)

// ControlRequest 是本地健康端点接受的一次 NDJSON 请求。
type ControlRequest struct {
	Method string `json:"method"`
}

// ControlResponse 是本地健康端点返回的一次 NDJSON 响应。
type ControlResponse struct {
	Status *Status `json:"status,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// StatusServer 在当前用户私有本地端点上提供只读 Supervisor 状态。
type StatusServer interface {
	Run(context.Context, string, func() Status) error
}

// StatusClient 查询一个正在运行的 Supervisor 状态。
type StatusClient interface {
	Status(context.Context, string) (Status, error)
}

// NewControlServer 创建当前平台的本地健康服务。
func NewControlServer() StatusServer {
	return newPlatformControlServer()
}

// NewControlClient 创建当前平台的本地健康客户端。
func NewControlClient() StatusClient {
	return newPlatformControlClient()
}
