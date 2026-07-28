package adminproto

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrorCode 是 HPAP/1 稳定错误码。
type ErrorCode string

const (
	CodeProtocolInvalidRequest     ErrorCode = "protocol.invalid_request"
	CodeProtocolUnsupportedVersion ErrorCode = "protocol.unsupported_version"
	CodeProtocolUnsupportedMethod  ErrorCode = "protocol.unsupported_method"
	CodeProtocolLimitExceeded      ErrorCode = "protocol.limit_exceeded"
	CodeArgumentInvalid            ErrorCode = "argument.invalid"
	CodeCredentialNotFound         ErrorCode = "credential.not_found"
	CodeCredentialConflict         ErrorCode = "credential.conflict"
	CodeCredentialSourceRequired   ErrorCode = "credential.source_required"
	CodeCredentialSourceInvalid    ErrorCode = "credential.source_invalid"
	CodeConnectionNotFound         ErrorCode = "connection.not_found"
	CodeServerBusy                 ErrorCode = "server.busy"
	CodeServerInternal             ErrorCode = "server.internal"
)

var errorCodes = [...]ErrorCode{
	CodeProtocolInvalidRequest,
	CodeProtocolUnsupportedVersion,
	CodeProtocolUnsupportedMethod,
	CodeProtocolLimitExceeded,
	CodeArgumentInvalid,
	CodeCredentialNotFound,
	CodeCredentialConflict,
	CodeCredentialSourceRequired,
	CodeCredentialSourceInvalid,
	CodeConnectionNotFound,
	CodeServerBusy,
	CodeServerInternal,
}

// Error 是 HPAP/1 返回给管理客户端的稳定业务错误。
type Error struct {
	Code    ErrorCode       `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

// CodecError 描述请求或响应在协议编解码阶段失败的稳定错误码。
type CodecError struct {
	Code    ErrorCode
	Message string
	cause   error
}

// Error 返回不包含原始帧内容的协议错误摘要。
func (err *CodecError) Error() string {
	if err == nil {
		return "HPAP 协议错误"
	}
	if err.Message == "" {
		return string(err.Code)
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

// Unwrap 返回底层解析错误，底层错误不得直接返回给远程调用方。
func (err *CodecError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// ErrorCodes 返回 HPAP/1 固定错误码，调用方可以安全修改返回切片。
func ErrorCodes() []ErrorCode {
	return append([]ErrorCode(nil), errorCodes[:]...)
}

// IsKnownErrorCode 报告错误码是否属于 HPAP/1 固定错误集合。
func IsKnownErrorCode(code ErrorCode) bool {
	for _, current := range errorCodes {
		if current == code {
			return true
		}
	}
	return false
}

// IsCode 报告错误链中是否包含指定 HPAP 稳定错误码。
func IsCode(err error, code ErrorCode) bool {
	var codecErr *CodecError
	return errors.As(err, &codecErr) && codecErr.Code == code
}

func newCodecError(code ErrorCode, message string, cause error) error {
	return &CodecError{Code: code, Message: message, cause: cause}
}
