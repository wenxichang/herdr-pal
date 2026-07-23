// Package herdr 提供 Herdr 本地 Socket API 的客户端基础能力。
package herdr

import (
	"errors"
	"fmt"
)

var (
	// ErrUnavailable 表示 Herdr Socket 当前不可连接或请求期间连接不可用。
	ErrUnavailable = errors.New("Herdr 不可用")
	// ErrFrameTooLarge 表示 Herdr NDJSON 单帧超过允许的最大长度。
	ErrFrameTooLarge = errors.New("Herdr NDJSON 帧过大")
	// ErrProtocol 表示 Herdr 返回了不符合 NDJSON 协议的内容。
	ErrProtocol = errors.New("Herdr 协议错误")
	// ErrProtocolMismatch 表示 Herdr 的协议版本与客户端不兼容。
	ErrProtocolMismatch = errors.New("Herdr 协议版本不匹配")
)

// APIError 表示 Herdr API 返回的业务错误。
type APIError struct {
	// Code 是 Herdr 返回的稳定业务错误码。
	Code string
	// Message 是 Herdr 返回的面向用户的错误说明。
	Message string
}

// Error 返回稳定的 Herdr API 错误描述。
func (e *APIError) Error() string {
	return fmt.Sprintf("Herdr API %s: %s", e.Code, e.Message)
}
