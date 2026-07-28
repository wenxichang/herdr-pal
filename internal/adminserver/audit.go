package adminserver

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

const auditTargetByteLimit = 160

func auditRequest(logger *slog.Logger, peerUID uint32, request adminproto.Request, target string, response adminproto.Response, duration time.Duration) {
	if logger == nil {
		return
	}
	fields := []any{
		"peer_uid", peerUID,
		"method", request.Method,
		"request_hash", auditRequestHash(request.ID),
		"duration", duration,
	}
	if target = sanitizeAuditTarget(target); target != "" {
		fields = append(fields, "target", target)
	}
	if response.Error == nil {
		fields = append(fields, "outcome", "success")
	} else {
		fields = append(fields, "outcome", "error", "error_code", response.Error.Code)
	}
	logger.Info("HPAP 管理请求", fields...)
}

func auditProtocolFailure(logger *slog.Logger, peerUID uint32, request adminproto.Request, code adminproto.ErrorCode, duration time.Duration) {
	if logger == nil {
		return
	}
	logger.Warn("HPAP 管理请求协议失败",
		"peer_uid", peerUID,
		"method", request.Method,
		"request_hash", auditRequestHash(request.ID),
		"error_code", code,
		"duration", duration,
	)
}

func auditBusyConnection(logger *slog.Logger, connection netPeerUID) {
	if logger == nil {
		return
	}
	logger.Warn("HPAP 管理连接被拒绝", "peer_uid", peerUIDOf(connection), "error_code", adminproto.CodeServerBusy)
}

func auditRequestHash(requestID string) string {
	if requestID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(requestID))
	return fmt.Sprintf("%x", digest[:8])
}

func sanitizeAuditTarget(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(value)
	if value == "" {
		return ""
	}
	for len(value) > auditTargetByteLimit {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
