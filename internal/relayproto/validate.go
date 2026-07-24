package relayproto

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var machineIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateClientHello 校验客户端自声明身份和版本字段。
func ValidateClientHello(hello ClientHello) error {
	if !validRequiredUTF8(hello.UserID, MaxUserIDBytes) {
		return ErrInvalidIdentity
	}
	if !machineIDPattern.MatchString(hello.MachineID) {
		return ErrInvalidIdentity
	}
	if hello.ClientVersion != "" && !validUTF8(hello.ClientVersion, MaxLabelBytes) {
		return ErrInvalidIdentity
	}
	return nil
}

// ValidateSnapshot 校验完整快照；任何条目失败都会拒绝整个快照。
func ValidateSnapshot(snapshot SessionSnapshot) error {
	if snapshot.Sequence == 0 {
		return ErrInvalidSnapshot
	}
	if len(snapshot.Sessions) > MaxSessionsPerSnapshot {
		return ErrLimitExceeded
	}
	localIndexes := make(map[int]struct{}, len(snapshot.Sessions))
	paneIDs := make(map[string]struct{}, len(snapshot.Sessions))
	for _, current := range snapshot.Sessions {
		if err := validateSession(current); err != nil {
			return err
		}
		if _, exists := localIndexes[current.LocalIndex]; exists {
			return fmt.Errorf("%w: 本地序号重复", ErrInvalidSnapshot)
		}
		if _, exists := paneIDs[current.PaneID]; exists {
			return fmt.Errorf("%w: pane 重复", ErrInvalidSnapshot)
		}
		localIndexes[current.LocalIndex] = struct{}{}
		paneIDs[current.PaneID] = struct{}{}
	}
	return nil
}

// ValidateSessionRef 校验跨机器选择使用的稳定目标引用。
func ValidateSessionRef(target SessionRef) error {
	if !machineIDPattern.MatchString(target.MachineID) || target.LocalIndex <= 0 ||
		!validRequiredUTF8(target.PaneID, MaxLabelBytes) ||
		!validRequiredUTF8(target.OccupantHash, MaxLabelBytes) {
		return ErrInvalidTarget
	}
	return nil
}

func validateSession(current Session) error {
	if current.LocalIndex <= 0 ||
		!validRequiredUTF8(current.PaneID, MaxLabelBytes) ||
		!validRequiredUTF8(current.TerminalID, MaxLabelBytes) ||
		!validRequiredUTF8(current.OccupantHash, MaxLabelBytes) ||
		!validUTF8(current.AgentSessionRef, MaxLabelBytes) ||
		!validUTF8(current.Agent, MaxLabelBytes) ||
		!validUTF8(current.DisplayAgent, MaxLabelBytes) ||
		!validUTF8(current.Title, MaxLabelBytes) ||
		!validUTF8(current.Workspace, MaxLabelBytes) ||
		!validUTF8(current.Tab, MaxLabelBytes) ||
		!validStatus(current.Status) {
		return ErrInvalidSnapshot
	}
	return nil
}

func validStatus(status string) bool {
	switch status {
	case "idle", "working", "blocked", "done", "unknown":
		return true
	default:
		return false
	}
}

func validRequiredUTF8(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && validUTF8(value, limit)
}

func validUTF8(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit
}
