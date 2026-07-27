package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

const serverLogReasonLimit = 240

func serverErrorLogArgs(err error) []any {
	args := []any{"error_type", serverErrorType(err), "reason", safeServerErrorReason(err)}
	var protocolError *wecom.ProtocolError
	if errors.As(err, &protocolError) && protocolError.ErrCode != 0 {
		args = append(args, "error_code", protocolError.ErrCode)
	}
	return args
}

func serverErrorType(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, ErrTargetChanged):
		return "target_changed"
	case errors.Is(err, ErrClientUnavailable):
		return "client_unavailable"
	case errors.Is(err, ErrClientQueueFull):
		return "client_queue_full"
	case errors.Is(err, ErrInflightFull):
		return "inflight_full"
	case errors.Is(err, ErrInvalidHubRequest):
		return "invalid_hub_request"
	case errors.Is(err, ErrDuplicateClient):
		return "duplicate_client"
	case errors.Is(err, ErrUnknownConnection):
		return "unknown_connection"
	case errors.Is(err, ErrSnapshotStale):
		return "snapshot_stale"
	case errors.Is(err, ErrNoListSnapshot):
		return "no_list_snapshot"
	case errors.Is(err, ErrSelectionIndexOutOfRange):
		return "selection_out_of_range"
	case errors.Is(err, ErrNoSelection):
		return "no_selection"
	case errors.Is(err, ErrUserQueueFull):
		return "user_queue_full"
	case errors.Is(err, wecom.ErrEventQueueFull):
		return "wecom_event_queue_full"
	case errors.Is(err, wecom.ErrProtocol):
		return "wecom_protocol"
	case errors.Is(err, wecom.ErrUnavailable):
		return "wecom_unavailable"
	case errors.Is(err, relayproto.ErrProtocolMismatch):
		return "protocol_mismatch"
	case errors.Is(err, relayproto.ErrFrameTooLarge):
		return "frame_too_large"
	case errors.Is(err, relayproto.ErrInvalidIdentity):
		return "invalid_identity"
	case errors.Is(err, relayproto.ErrInvalidSnapshot):
		return "invalid_snapshot"
	case errors.Is(err, relayproto.ErrInvalidTarget):
		return "invalid_target"
	case errors.Is(err, relayproto.ErrLimitExceeded):
		return "limit_exceeded"
	case errors.Is(err, relayproto.ErrInvalidFrame):
		return "invalid_frame"
	default:
		return "runtime_error"
	}
}

func safeServerErrorReason(err error) string {
	if err == nil {
		return "无错误"
	}
	reason := strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(err.Error(), "�"))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "错误未提供原因"
	}
	if len(reason) > serverLogReasonLimit {
		reason = reason[:serverLogReasonLimit] + "…"
	}
	return reason
}

func connectionLogArgs(connection *clientConnection) []any {
	if connection == nil {
		return nil
	}
	return []any{
		"user_hash", routerHash(connection.key.UserID),
		"machine_id", safeLogValue(connection.key.MachineID),
		"connection_id", connection.id,
	}
}

func targetLogArgs(target relayproto.SessionRef) []any {
	return []any{
		"machine_id", safeLogValue(target.MachineID),
		"local_index", target.LocalIndex,
		"pane_id", safeLogValue(target.PaneID),
		"occupant_hash", routerHash(target.OccupantHash),
	}
}

func safeLogValue(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(value, "�"))
	value = strings.TrimSpace(value)
	if len(value) > serverLogReasonLimit {
		return fmt.Sprintf("%s…", value[:serverLogReasonLimit])
	}
	return value
}

func serverActionName(kind serverActionKind) string {
	switch kind {
	case serverActionUserID:
		return "userid"
	case serverActionList:
		return "list"
	case serverActionSelect:
		return "select"
	case serverActionHelp:
		return "help"
	case serverActionDirected:
		return "directed_execute"
	default:
		return "execute"
	}
}
