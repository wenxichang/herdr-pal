// Package adminservice 提供 HPAP 与 Web 管理入口共享的协议无关业务规则。
package adminservice

import (
	"errors"
	"fmt"
)

// ErrorCode 是管理领域供传输适配器映射的稳定错误类别。
type ErrorCode string

const (
	CodeInvalidArgument            ErrorCode = "invalid_argument"
	CodeCredentialNotFound         ErrorCode = "credential_not_found"
	CodeCredentialConflict         ErrorCode = "credential_conflict"
	CodeSourceRequired             ErrorCode = "source_required"
	CodeSourceInvalid              ErrorCode = "source_invalid"
	CodeConnectionNotFound         ErrorCode = "connection_not_found"
	CodeRegistrationNotFound       ErrorCode = "registration_not_found"
	CodeRegistrationConflict       ErrorCode = "registration_conflict"
	CodeRegistrationDeliveryFailed ErrorCode = "registration_delivery_failed"
	CodeRegistrationRollbackFailed ErrorCode = "registration_rollback_failed"
	CodeRegistrationCleanupFailed  ErrorCode = "registration_cleanup_failed"
	CodeServerBusy                 ErrorCode = "server_busy"
	CodeInternal                   ErrorCode = "internal"
)

// Error 是不向调用方泄露持久化路径或底层错误正文的管理领域错误。
type Error struct {
	Code    ErrorCode
	Message string
	cause   error
}

// Error 返回可安全展示的错误说明。
func (err *Error) Error() string {
	if err == nil {
		return "管理操作失败"
	}
	if err.Message == "" {
		return string(err.Code)
	}
	return err.Message
}

// Unwrap 返回仅供本地诊断的底层错误。
func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// ErrorCodeOf 返回错误链中的稳定领域错误码。
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var serviceErr *Error
	if errors.As(err, &serviceErr) {
		return serviceErr.Code
	}
	return CodeInternal
}

func newError(code ErrorCode, message string, cause error) error {
	if code == "" {
		code = CodeInternal
	}
	if message == "" {
		message = fmt.Sprintf("管理操作失败：%s", code)
	}
	return &Error{Code: code, Message: message, cause: cause}
}
