package hprp

// Outcome 是请求结果的稳定顶层分类。
type Outcome string

const (
	OutcomeOK            Outcome = "ok"
	OutcomeRejected      Outcome = "rejected"
	OutcomeFailed        Outcome = "failed"
	OutcomeCancelled     Outcome = "cancelled"
	OutcomeIndeterminate Outcome = "indeterminate"
)

// ErrorCode 是程序可以稳定决策的 HPRP 错误码。
type ErrorCode string

const (
	CodeProtocolInvalidMessage               ErrorCode = "protocol.invalid_message"
	CodeProtocolUnsupportedType              ErrorCode = "protocol.unsupported_type"
	CodeProtocolRequiredExtensionUnsupported ErrorCode = "protocol.required_extension_unsupported"
	CodeProtocolLimitExceeded                ErrorCode = "protocol.limit_exceeded"
	CodeSyncStaleSnapshot                    ErrorCode = "sync.stale_snapshot"
	CodeSyncResyncRequired                   ErrorCode = "sync.resync_required"
	CodeTargetNotFound                       ErrorCode = "target.not_found"
	CodeTargetSessionChanged                 ErrorCode = "target.session_changed"
	CodeTargetNotReady                       ErrorCode = "target.not_ready"
	CodeCommandUnsupported                   ErrorCode = "command.unsupported"
	CodeCommandDenied                        ErrorCode = "command.denied"
	CodeCommandTimeout                       ErrorCode = "command.timeout"
	CodeCommandExecutionFailed               ErrorCode = "command.execution_failed"
	CodeFeatureUnsupported                   ErrorCode = "feature.unsupported"
	CodeFeatureInvalidInput                  ErrorCode = "feature.invalid_input"
	CodeFeatureIdempotencyConflict           ErrorCode = "feature.idempotency_conflict"
	CodeFeatureNotCancellable                ErrorCode = "feature.not_cancellable"
	CodeFeatureNotRunning                    ErrorCode = "feature.not_running"
	CodeFeatureExecutionFailed               ErrorCode = "feature.execution_failed"
	CodeServerBusy                           ErrorCode = "server.busy"
	CodeServerInternal                       ErrorCode = "server.internal"
)

// Error 是结果消息携带的稳定机器错误和可选展示信息。
type Error struct {
	Code         ErrorCode      `json:"code"`
	Message      string         `json:"message,omitempty"`
	Retryable    bool           `json:"retryable"`
	RetryAfterMS int64          `json:"retry_after_ms,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

// ProtocolError 是连接状态机或消息信封错误的公共负载。
type ProtocolError struct {
	Error Error `json:"error"`
	Close bool  `json:"close"`
}
