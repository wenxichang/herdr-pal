// Package adminproto 定义 hp-cli 与 herdr-pal-server 之间的 HPAP/1 本地管理协议。
package adminproto

import "encoding/json"

const (
	// Protocol 是首版 Herdr Pal Administration Protocol 标识。
	Protocol = "HPAP/1"
	// MaxFrameBytes 是单条 HPAP NDJSON 帧允许的最大字节数。
	MaxFrameBytes = 1 << 20
)

// Request 是一条 HPAP/1 管理请求信封。
type Request struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Method   Method          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Response 是一条 HPAP/1 管理响应信封。
//
// Result 与 Error 必须且只能设置一个。
type Response struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}
