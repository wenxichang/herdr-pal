// Package policy 为所有进入 Herdr 的外部输入提供最小安全边界。
package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	// ErrInvalidAllowedUserID 表示授权用户配置为空。
	ErrInvalidAllowedUserID = errors.New("授权用户配置无效")
	// ErrUnauthorized 表示外部身份不具备输入权限。
	ErrUnauthorized = errors.New("输入身份未获授权")
	// ErrInvalidKey 表示解析后的按键不在安全白名单内。
	ErrInvalidKey = errors.New("按键不受支持")
	// ErrInvalidAudit 表示按键审计记录包含不安全或不完整的数据。
	ErrInvalidAudit = errors.New("按键审计记录无效")
)

// Identity 是 IM 适配器验证后提供的用户与会话类型。
type Identity struct {
	// UserID 是 IM 平台的稳定用户标识。
	UserID string
	// ChatType 是 IM 会话类型；当前仅允许 single。
	ChatType string
}

// Guard 负责校验进入 Herdr 前的身份和显式按键。
type Guard struct {
	allowedUserID string
}

// NewGuard 创建仅允许 allowedUserID 私聊输入的策略守卫。
func NewGuard(allowedUserID string) (*Guard, error) {
	if strings.TrimSpace(allowedUserID) == "" {
		return nil, ErrInvalidAllowedUserID
	}
	return &Guard{allowedUserID: allowedUserID}, nil
}

// Authorize 仅在身份精确匹配配置用户且来自 single 会话时返回成功。
func (g *Guard) Authorize(identity Identity) error {
	if g == nil || identity.ChatType != "single" || identity.UserID == "" || identity.UserID != g.allowedUserID {
		return ErrUnauthorized
	}
	return nil
}

// ValidateKey 验证解析后的按键是否属于允许发送给 Agent 的最小白名单。
func (g *Guard) ValidateKey(key string) error {
	if !allowedKey(key) {
		return ErrInvalidKey
	}
	return nil
}

func allowedKey(key string) bool {
	switch key {
	case "up", "down", "enter", "esc", "space":
		return true
	}
	if len(key) != 1 {
		return false
	}
	return key[0] >= 'A' && key[0] <= 'Z' ||
		key[0] >= 'a' && key[0] <= 'z' ||
		key[0] >= '0' && key[0] <= '9'
}

// AuditResult 是按键处理结果的封闭短标识。
type AuditResult string

const (
	// AuditResultSent 表示按键已发送给 Herdr。
	AuditResultSent AuditResult = "sent"
	// AuditResultRejected 表示按键在发送前被策略拒绝。
	AuditResultRejected AuditResult = "rejected"
	// AuditResultFailed 表示发送尝试失败。
	AuditResultFailed AuditResult = "failed"
)

// KeyAudit 是一次显式按键处理的最小、不可变可审计记录。
//
// 它刻意不保存 Secret、token、提示词或终端正文；调用方可将其交给后续日志或持久化层。
type KeyAudit struct {
	userID       string
	paneID       string
	occupantHash string
	key          string
	at           time.Time
	result       AuditResult
}

// NewKeyAudit 创建只包含允许审计字段的按键记录。
func NewKeyAudit(userID, paneID, occupantHash, key string, at time.Time, result AuditResult) (KeyAudit, error) {
	audit := KeyAudit{
		userID:       userID,
		paneID:       paneID,
		occupantHash: occupantHash,
		key:          key,
		at:           at,
		result:       result,
	}
	if err := audit.validate(); err != nil {
		return KeyAudit{}, err
	}
	return audit, nil
}

// UserID 返回执行动作的 IM 用户标识。
func (a KeyAudit) UserID() string { return a.userID }

// PaneID 返回动作目标的 Herdr pane 标识。
func (a KeyAudit) PaneID() string { return a.paneID }

// OccupantHash 返回目标 Agent occupant 的 SHA-256 摘要。
func (a KeyAudit) OccupantHash() string { return a.occupantHash }

// Key 返回已通过白名单验证的规范化按键。
func (a KeyAudit) Key() string { return a.key }

// At 返回动作处理时间。
func (a KeyAudit) At() time.Time { return a.at }

// Result 返回按键处理结果的受限短标识。
func (a KeyAudit) Result() AuditResult { return a.result }

// MarshalJSON 只序列化允许审计的六个字段，并在记录不完整时拒绝输出。
func (a KeyAudit) MarshalJSON() ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		UserID       string      `json:"user_id"`
		PaneID       string      `json:"pane_id"`
		OccupantHash string      `json:"occupant_hash"`
		Key          string      `json:"key"`
		At           time.Time   `json:"at"`
		Result       AuditResult `json:"result"`
	}{
		UserID:       a.userID,
		PaneID:       a.paneID,
		OccupantHash: a.occupantHash,
		Key:          a.key,
		At:           a.at,
		Result:       a.result,
	})
}

func (a KeyAudit) validate() error {
	if strings.TrimSpace(a.userID) == "" || strings.TrimSpace(a.paneID) == "" ||
		!isSHA256Hex(a.occupantHash) || !allowedKey(a.key) || a.at.IsZero() || !isAuditResult(a.result) {
		return errors.Join(ErrInvalidAudit, invalidAuditKeyError(a.key))
	}
	return nil
}

func invalidAuditKeyError(key string) error {
	if !allowedKey(key) {
		return ErrInvalidKey
	}
	return nil
}

func isAuditResult(result AuditResult) bool {
	switch result {
	case AuditResultSent, AuditResultRejected, AuditResultFailed:
		return true
	default:
		return false
	}
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := range value {
		if value[index] >= '0' && value[index] <= '9' || value[index] >= 'a' && value[index] <= 'f' {
			continue
		}
		return false
	}
	return true
}
