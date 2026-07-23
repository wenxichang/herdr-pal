// Package policy 为所有进入 Herdr 的外部输入提供最小安全边界。
package policy

import (
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

// KeyAudit 是一次显式按键处理的最小可审计记录。
//
// 它刻意不保存 Secret、token、提示词或终端正文；调用方可将其交给后续日志或持久化层。
type KeyAudit struct {
	// UserID 是执行动作的 IM 用户标识。
	UserID string `json:"user_id"`
	// PaneID 是动作目标的 Herdr pane 标识。
	PaneID string `json:"pane_id"`
	// OccupantHash 是目标 Agent occupant 的摘要。
	OccupantHash string `json:"occupant_hash"`
	// Key 是已通过白名单验证的规范化按键。
	Key string `json:"key"`
	// At 是动作处理时间。
	At time.Time `json:"at"`
	// Result 是动作处理结果的短标识。
	Result string `json:"result"`
}

// NewKeyAudit 创建只包含允许审计字段的按键记录。
func NewKeyAudit(userID, paneID, occupantHash, key string, at time.Time, result string) (KeyAudit, error) {
	if !allowedKey(key) {
		return KeyAudit{}, ErrInvalidKey
	}
	return KeyAudit{
		UserID:       userID,
		PaneID:       paneID,
		OccupantHash: occupantHash,
		Key:          key,
		At:           at,
		Result:       result,
	}, nil
}
