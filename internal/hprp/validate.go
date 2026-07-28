package hprp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxSessions           = 256
	MaxMachineIDBytes     = 64
	MaxLabelBytes         = 512
	MaxExtensionNameBytes = 128
	MaxContentBytes       = 1 << 18
)

var (
	ErrInvalidTarget   = errors.New("HPRP 稳定目标无效")
	ErrInvalidSnapshot = errors.New("HPRP 会话快照无效")
	ErrInvalidOutcome  = errors.New("HPRP 结果分类无效")
)

var (
	machineIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	versionedNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*\.v[1-9][0-9]*$`)
	errorCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
)

// ValidateClientHello 校验 Pal 的实现信息、扩展声明和资源限制。
func ValidateClientHello(hello ClientHello) error {
	if !validRequiredLabel(hello.Implementation.Name) || !validRequiredLabel(hello.Implementation.Version) ||
		!validRequiredLabel(hello.Implementation.OS) || !validRequiredLabel(hello.Implementation.Arch) {
		return fmt.Errorf("%w: implementation 无效", ErrInvalidMessage)
	}
	if err := validateVersionedList(hello.Capabilities); err != nil {
		return err
	}
	if err := validateFeatureOffers(hello.Features); err != nil {
		return err
	}
	if hello.Limits.MaxReceiveMessageBytes <= 0 || hello.Limits.MaxInflightCommands <= 0 ||
		hello.Limits.MaxInflightFeatures < 0 || hello.Limits.IdempotencyWindowMS <= 0 {
		return fmt.Errorf("%w: limits 无效", ErrInvalidMessage)
	}
	for key, value := range hello.Diagnostics {
		if !validRequiredLabel(key) || !validLabel(value) {
			return fmt.Errorf("%w: diagnostics 无效", ErrInvalidMessage)
		}
	}
	return nil
}

// ValidateServerHello 校验 Server 返回的身份、协商结果和连接限制。
func ValidateServerHello(hello ServerHello) error {
	if !validRequiredLabel(hello.ConnectionID) || !machineIDPattern.MatchString(hello.MachineID) {
		return fmt.Errorf("%w: server identity 无效", ErrInvalidMessage)
	}
	if err := validateVersionedList(hello.Capabilities); err != nil {
		return err
	}
	if err := validateFeatureOffers(hello.Features); err != nil {
		return err
	}
	if hello.Limits.MaxMessageBytes <= 0 || hello.Limits.MaxSessions <= 0 ||
		hello.Limits.MaxInflightCommands <= 0 || hello.Limits.MaxInflightFeatures < 0 ||
		hello.Limits.MaxOutputBytes <= 0 || hello.Limits.IdempotencyWindowMS <= 0 ||
		hello.Heartbeat.PingIntervalMS <= 0 || hello.Heartbeat.IdleTimeoutMS <= 0 {
		return fmt.Errorf("%w: server limits 无效", ErrInvalidMessage)
	}
	return nil
}

// ValidateTarget 校验机器、物理 slot 和逻辑 session 三层稳定身份。
func ValidateTarget(target Target) error {
	if ValidateMachineID(target.MachineID) != nil || !validRequiredLabel(target.SlotID) ||
		!validRequiredLabel(target.SessionID) {
		return ErrInvalidTarget
	}
	return nil
}

// ValidateMachineID 校验由终端凭据绑定的逻辑机器标识。
func ValidateMachineID(machineID string) error {
	if !machineIDPattern.MatchString(machineID) {
		return ErrInvalidTarget
	}
	return nil
}

// ValidateSessionSnapshot 校验完整快照结构；未知状态值由接收方降级为 unknown。
func ValidateSessionSnapshot(snapshot SessionSnapshot) error {
	if snapshot.Sequence == 0 || len(snapshot.Sessions) > MaxSessions {
		return ErrInvalidSnapshot
	}
	slots := make(map[string]struct{}, len(snapshot.Sessions))
	indexes := make(map[int]struct{}, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		if !validRequiredLabel(session.SlotID) || !validRequiredLabel(session.SessionID) ||
			session.Display.Index <= 0 || !validSessionStatus(session.Status) ||
			!validLabel(session.Display.Agent) || !validLabel(session.Display.DisplayAgent) ||
			!validLabel(session.Display.Workspace) || !validLabel(session.Display.Tab) || !validLabel(session.Display.Title) {
			return ErrInvalidSnapshot
		}
		if _, exists := slots[session.SlotID]; exists {
			return ErrInvalidSnapshot
		}
		if _, exists := indexes[session.Display.Index]; exists {
			return ErrInvalidSnapshot
		}
		slots[session.SlotID] = struct{}{}
		indexes[session.Display.Index] = struct{}{}
	}
	return nil
}

// NormalizeStatus 将 HPRP/1 未知 Agent 状态保守降级为 unknown。
func NormalizeStatus(status string) string {
	switch status {
	case StatusIdle, StatusWorking, StatusBlocked, StatusDone, StatusUnknown:
		return status
	default:
		return StatusUnknown
	}
}

// ValidateCommandResult 校验基础命令允许的 outcome 和可选替换目标。
func ValidateCommandResult(result CommandResult) error {
	if !oneOfOutcome(result.Outcome, OutcomeOK, OutcomeRejected, OutcomeFailed, OutcomeIndeterminate) {
		return ErrInvalidOutcome
	}
	if result.Content != nil {
		if err := validateTextContent(*result.Content); err != nil {
			return err
		}
	}
	if result.ReplacementTarget != nil {
		if err := ValidateTarget(*result.ReplacementTarget); err != nil {
			return err
		}
	}
	return validateResultError(result.Outcome, result.Error)
}

// ValidateCommandOutput 校验有序命令输出及其稳定来源。
func ValidateCommandOutput(output CommandOutput) error {
	if output.Sequence == 0 {
		return fmt.Errorf("%w: command output sequence 无效", ErrInvalidMessage)
	}
	if err := ValidateTarget(output.Target); err != nil {
		return err
	}
	return validateTextContent(output.Content)
}

// ValidateFeatureResult 校验 Feature 版本和通用最终 outcome。
func ValidateFeatureResult(result FeatureResult) error {
	if !validVersionedName(result.Feature) {
		return fmt.Errorf("%w: Feature 名称无效", ErrInvalidMessage)
	}
	if !oneOfOutcome(result.Outcome, OutcomeOK, OutcomeRejected, OutcomeFailed, OutcomeCancelled, OutcomeIndeterminate) {
		return ErrInvalidOutcome
	}
	return validateResultError(result.Outcome, result.Error)
}

// ValidateFeatureCancelResult 校验取消请求自身允许的 outcome。
func ValidateFeatureCancelResult(result FeatureCancelResult) error {
	if !oneOfOutcome(result.Outcome, OutcomeOK, OutcomeRejected, OutcomeFailed) {
		return ErrInvalidOutcome
	}
	return validateResultError(result.Outcome, result.Error)
}

func validateTextContent(content TextContent) error {
	if content.Type != ContentTypeText || !utf8.ValidString(content.Text) || len(content.Text) > MaxContentBytes {
		return fmt.Errorf("%w: text content 无效", ErrInvalidMessage)
	}
	return nil
}

func validateResultError(outcome Outcome, resultError *Error) error {
	if outcome == OutcomeOK && resultError != nil {
		return fmt.Errorf("%w: 成功结果不能包含 error", ErrInvalidMessage)
	}
	if resultError == nil {
		return nil
	}
	if !errorCodePattern.MatchString(string(resultError.Code)) || !validLabel(resultError.Message) || resultError.RetryAfterMS < 0 {
		return fmt.Errorf("%w: error 无效", ErrInvalidMessage)
	}
	return nil
}

func validateVersionedList(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !validVersionedName(name) {
			return fmt.Errorf("%w: 扩展名称无效", ErrInvalidMessage)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: 扩展名称重复", ErrInvalidMessage)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateFeatureOffers(features map[string]FeatureOffer) error {
	for name, offer := range features {
		if !validVersionedName(name) {
			return fmt.Errorf("%w: Feature 名称无效", ErrInvalidMessage)
		}
		for parameter, raw := range offer.Parameters {
			if !validRequiredLabel(parameter) || len(raw) == 0 || !json.Valid(raw) {
				return fmt.Errorf("%w: Feature 参数无效", ErrInvalidMessage)
			}
			if err := rejectDuplicateJSONFields(bytes.TrimSpace(raw)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validVersionedName(name string) bool {
	return len(name) <= MaxExtensionNameBytes && versionedNamePattern.MatchString(name)
}

func splitVersionedName(name string) (string, int, bool) {
	if !validVersionedName(name) {
		return "", 0, false
	}
	index := strings.LastIndex(name, ".v")
	version, err := strconv.Atoi(name[index+2:])
	if err != nil || version < 1 {
		return "", 0, false
	}
	return name[:index], version, true
}

func validSessionStatus(status string) bool {
	return strings.TrimSpace(status) != "" && utf8.ValidString(status) && len(status) <= 64
}

func validRequiredLabel(value string) bool {
	return strings.TrimSpace(value) != "" && validLabel(value)
}

func validLabel(value string) bool {
	return utf8.ValidString(value) && len(value) <= MaxLabelBytes
}

func oneOfOutcome(value Outcome, allowed ...Outcome) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
